package payment

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Ledger accounts. Rent is split into three claims so each is traceable:
//   - organizer_escrow: the organizer's share of the rent, held until payout (E09).
//   - platform_revenue: the platform fee and paid plan prices.
//   - provider_fee_payable: the per-method admin fee owed to Midtrans, collected from the tenant.
//   - provider_clearing: money held by Midtrans on our behalf.
const (
	accountClearing    = "provider_clearing"
	accountEscrow      = "organizer_escrow"
	accountRevenue     = "platform_revenue"
	accountProviderFee = "provider_fee_payable"
)

// Ledger kinds keep received payments, plan prices, reversals, refunds, and payouts apart.
const (
	kindPayment     = "payment_received"
	kindPlanPayment = "plan_payment_received"
	kindReversal    = "reversal"
	kindRefund      = "refund_issued"
	kindPayout      = "payout_released"
)

// Entry is one side of a ledger transaction.
type Entry struct {
	Account   string
	Direction string // debit or credit
	Amount    int64
}

var (
	ErrAlreadyPosted    = errors.New("ledger transaction already exists for this reference")
	ErrNothingToReverse = errors.New("no ledger transaction to reverse")
)

// post writes one append-only ledger transaction. The deferred balance trigger rejects unbalanced
// entries at commit. Entries of zero are skipped. It returns the new transaction ID, or "" when the
// same (kind, ref) was already posted, which makes callers idempotent.
func post(ctx context.Context, tx pgx.Tx, kind, refType, refID string, reverses *string, entries []Entry) (string, error) {
	var txID string
	err := tx.QueryRow(ctx, `INSERT INTO ledger_transactions (kind, ref_type, ref_id, reverses)
		VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING RETURNING id`, kind, refType, refID, reverses).Scan(&txID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if entry.Amount == 0 {
			continue
		}
		if _, err := tx.Exec(ctx, `INSERT INTO ledger_entries (transaction_id, account, direction, amount)
			VALUES ($1, $2, $3, $4)`, txID, entry.Account, entry.Direction, entry.Amount); err != nil {
			return "", err
		}
	}
	return txID, nil
}

// recordPayment books a settled rent payment: the tenant's total sits in provider clearing and is
// claimed by the organizer (rent), the platform (platform fee), and the provider (method fee).
func recordPayment(ctx context.Context, tx pgx.Tx, attemptID string, rent, platformFee, methodFee int64) error {
	_, err := post(ctx, tx, kindPayment, "payment_attempt", attemptID, nil, []Entry{
		{accountClearing, "debit", rent + platformFee + methodFee},
		{accountEscrow, "credit", rent},
		{accountRevenue, "credit", platformFee},
		{accountProviderFee, "credit", methodFee},
	})
	return err
}

// recordPlanPayment books a settled plan purchase as platform revenue only.
func recordPlanPayment(ctx context.Context, tx pgx.Tx, attemptID string, amount int64) error {
	_, err := post(ctx, tx, kindPlanPayment, "payment_attempt", attemptID, nil, []Entry{
		{accountClearing, "debit", amount},
		{accountRevenue, "credit", amount},
	})
	return err
}

// RecordRefund books money returned to a tenant (E09). It leaves the organizer's escrow and the provider balance.
func RecordRefund(ctx context.Context, tx pgx.Tx, refundID string, amount int64) error {
	_, err := post(ctx, tx, kindRefund, "refund", refundID, nil, []Entry{
		{accountEscrow, "debit", amount}, {accountClearing, "credit", amount},
	})
	return err
}

// RecordPayout books a payout of released rent to the organizer's bank account (E09).
func RecordPayout(ctx context.Context, tx pgx.Tx, payoutID string, amount int64) error {
	_, err := post(ctx, tx, kindPayout, "payout", payoutID, nil, []Entry{
		{accountEscrow, "debit", amount}, {accountClearing, "credit", amount},
	})
	return err
}

// ReversePayment cancels a payment's ledger entries with a mirror transaction, for example after a
// chargeback or when a payment was recorded in error. Existing rows are never changed.
func (s Store) ReversePayment(ctx context.Context, attemptID string) error {
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var original string
		err := tx.QueryRow(ctx, `SELECT id FROM ledger_transactions WHERE ref_type = 'payment_attempt' AND ref_id = $1
			AND kind IN ('payment_received', 'plan_payment_received')`, attemptID).Scan(&original)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNothingToReverse
		}
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT account, direction, amount FROM ledger_entries WHERE transaction_id = $1`, original)
		if err != nil {
			return err
		}
		entries, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Entry, error) {
			var e Entry
			err := row.Scan(&e.Account, &e.Direction, &e.Amount)
			if e.Direction == "debit" {
				e.Direction = "credit"
			} else {
				e.Direction = "debit"
			}
			return e, err
		})
		if err != nil {
			return err
		}
		txID, err := post(ctx, tx, kindReversal, "payment_attempt", attemptID, &original, entries)
		if err != nil {
			return err
		}
		if txID == "" {
			return ErrAlreadyPosted
		}
		return nil
	})
}

// Balances returns the net debit (positive) or credit (negative) of every ledger account.
func (s Store) Balances(ctx context.Context) (map[string]int64, error) {
	rows, err := s.DB.Query(ctx, `SELECT account, SUM(CASE direction WHEN 'debit' THEN amount ELSE -amount END)::bigint
		FROM ledger_entries GROUP BY account`)
	if err != nil {
		return nil, err
	}
	result := map[string]int64{}
	for rows.Next() {
		var account string
		var net int64
		if err := rows.Scan(&account, &net); err != nil {
			return nil, err
		}
		result[account] = net
	}
	return result, rows.Err()
}

func (e Entry) String() string { return fmt.Sprintf("%s %s %d", e.Direction, e.Account, e.Amount) }
