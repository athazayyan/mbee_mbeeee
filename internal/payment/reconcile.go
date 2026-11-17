package payment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// unopenedGrace is how long past expiry an attempt may stay unknown to the provider before it is closed.
// Snap creates its transaction only when the payer picks a method, so "unknown" is normal until then.
const unopenedGrace = 10 * time.Minute

// ReconcileReport summarizes one reconciliation pass.
type ReconcileReport struct {
	Checked        int              `json:"checked"`
	Synced         int              `json:"synced"`
	Closed         int              `json:"closed"`
	LedgerMissing  int              `json:"ledger_missing"`
	LedgerMismatch int              `json:"ledger_mismatch"`
	OpenExceptions int              `json:"open_exceptions"`
	Balanced       bool             `json:"balanced"`
	Balances       map[string]int64 `json:"balances"`
}

// Recheck asks the provider for the real status of one attempt and applies it through the webhook path, so a
// repeat is harmless (E16-03). It is what an admin's retry runs.
func (s Store) Recheck(ctx context.Context, attemptID string) error {
	if s.Provider == nil {
		return ErrUnavailable
	}
	verified, err := s.Provider.Status(ctx, attemptID)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(verified)
	if err != nil {
		return err
	}
	return s.ApplyNotification(ctx, verified, payload)
}

// Reconcile heals missed webhooks and verifies the ledger against payment attempts.
//
//  1. Attempts still open after staleAfter are checked against the provider's status API and applied
//     through the same path as a webhook, so the result is identical and repeated runs change nothing.
//  2. Paid attempts without a ledger transaction, and ledger transactions whose clearing debit differs
//     from the attempt amount, are queued as exceptions for a person to resolve.
//  3. The ledger as a whole must balance.
func (s Store) Reconcile(ctx context.Context, staleAfter time.Duration, limit int) (ReconcileReport, error) {
	var report ReconcileReport
	if s.DB == nil {
		return report, ErrUnavailable
	}
	type open struct {
		id      string
		amount  int64
		expires time.Time
	}
	rows, err := s.DB.Query(ctx, `SELECT id, amount, expires_at FROM payment_attempts
		WHERE status IN ('created', 'pending') AND updated_at < clock_timestamp() - $1::interval
		ORDER BY updated_at LIMIT $2`, staleAfter.String(), limit)
	if err != nil {
		return report, err
	}
	stale, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (open, error) {
		var o open
		err := row.Scan(&o.id, &o.amount, &o.expires)
		return o, err
	})
	if err != nil {
		return report, err
	}
	for _, attempt := range stale {
		report.Checked++
		if s.Provider == nil {
			continue
		}
		verified, err := s.Provider.Status(ctx, attempt.id)
		if err == nil {
			payload, _ := json.Marshal(verified)
			if err := s.ApplyNotification(ctx, verified, payload); err != nil {
				return report, err
			}
			report.Synced++
			continue
		}
		if !errors.Is(err, ErrProvider) || time.Now().Before(attempt.expires.Add(unopenedGrace)) {
			continue
		}
		// Never opened at the provider and well past its expiry: close it locally.
		closing := Notification{
			OrderID: attempt.id, StatusCode: "407", TransactionStatus: "expire", Currency: "IDR",
			GrossAmount: fmt.Sprintf("%d.00", attempt.amount), TransactionID: "reconcile",
		}
		payload, _ := json.Marshal(closing)
		if err := s.ApplyNotification(ctx, closing, payload); err != nil {
			return report, err
		}
		report.Closed++
	}

	report.LedgerMissing, err = s.flagAttempts(ctx, "ledger_missing", `
		SELECT a.id, a.booking_id FROM payment_attempts a
		WHERE a.status = 'paid'
		  AND NOT EXISTS (SELECT 1 FROM ledger_transactions t WHERE t.ref_type = 'payment_attempt' AND t.ref_id = a.id
		      AND t.kind IN ('payment_received', 'plan_payment_received'))
		  AND NOT EXISTS (SELECT 1 FROM payment_exceptions x WHERE x.attempt_id = a.id
		      AND x.kind IN ('late_payment', 'duplicate_payment', 'amount_mismatch', 'ledger_mismatch'))`)
	if err != nil {
		return report, err
	}
	report.LedgerMismatch, err = s.flagAttempts(ctx, "ledger_mismatch", `
		SELECT a.id, a.booking_id FROM payment_attempts a
		JOIN ledger_transactions t ON t.ref_type = 'payment_attempt' AND t.ref_id = a.id
		  AND t.kind IN ('payment_received', 'plan_payment_received')
		WHERE (SELECT COALESCE(SUM(amount), 0) FROM ledger_entries
		       WHERE transaction_id = t.id AND account = 'provider_clearing' AND direction = 'debit') <> a.amount
		  AND NOT EXISTS (SELECT 1 FROM ledger_transactions r WHERE r.kind = 'reversal' AND r.reverses = t.id)`)
	if err != nil {
		return report, err
	}
	if report.Balances, err = s.Balances(ctx); err != nil {
		return report, err
	}
	var sum int64
	for _, net := range report.Balances {
		sum += net
	}
	report.Balanced = sum == 0
	err = s.DB.QueryRow(ctx, `SELECT count(*) FROM payment_exceptions WHERE status = 'open'`).Scan(&report.OpenExceptions)
	return report, err
}

// flagAttempts queues one open exception per attempt returned by query, skipping attempts already flagged.
func (s Store) flagAttempts(ctx context.Context, kind, query string) (int, error) {
	rows, err := s.DB.Query(ctx, query)
	if err != nil {
		return 0, err
	}
	type ref struct {
		attempt string
		booking *string
	}
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ref, error) {
		var r ref
		err := row.Scan(&r.attempt, &r.booking)
		return r, err
	})
	if err != nil {
		return 0, err
	}
	flagged := 0
	for _, item := range found {
		tag, err := s.DB.Exec(ctx, `INSERT INTO payment_exceptions (attempt_id, booking_id, kind, detail)
			SELECT $1, $2, $3, '{"source":"reconcile"}'::jsonb WHERE NOT EXISTS (
				SELECT 1 FROM payment_exceptions WHERE attempt_id = $1 AND kind = $3 AND status = 'open')`,
			item.attempt, item.booking, kind)
		if err != nil {
			return flagged, err
		}
		flagged += int(tag.RowsAffected())
	}
	return flagged, nil
}

// ResolveException marks an exception handled once a person has dealt with it.
func (s Store) ResolveException(ctx context.Context, id string) error {
	tag, err := s.DB.Exec(ctx, `UPDATE payment_exceptions SET status = 'resolved' WHERE id = $1 AND status = 'open'`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return pgx.ErrNoRows
	}
	return nil
}
