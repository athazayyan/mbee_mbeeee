package funds

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

// Dispute is a tenant's complaint about a paid booking. While one is open the event's payout is blocked.
type Dispute struct {
	ID             string     `json:"id"`
	BookingID      string     `json:"booking_id"`
	EventID        string     `json:"event_id"`
	Reason         string     `json:"reason"`
	Status         string     `json:"status"`
	ResolutionNote *string    `json:"resolution_note,omitempty"`
	RefundAmount   *int64     `json:"refund_amount,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	ResolvedAt     *time.Time `json:"resolved_at,omitempty"`
}

const disputeColumns = `d.id::text, d.booking_id::text, d.event_id::text, d.reason, d.status, d.resolution_note,
	d.refund_amount, d.created_at, d.resolved_at`

func scanDispute(row pgx.Row) (Dispute, error) {
	var d Dispute
	err := row.Scan(&d.ID, &d.BookingID, &d.EventID, &d.Reason, &d.Status, &d.ResolutionNote, &d.RefundAmount, &d.CreatedAt, &d.ResolvedAt)
	return d, err
}

// OpenDispute lets the tenant who paid raise a dispute until the event's payout has been released.
func (s Store) OpenDispute(ctx context.Context, actor, bookingID, reason string) (Dispute, error) {
	reason = strings.TrimSpace(reason)
	if !httpapi.ValidID(actor) || !httpapi.ValidID(bookingID) {
		return Dispute{}, ErrNotFound
	}
	if utf8.RuneCountInString(reason) < 1 || utf8.RuneCountInString(reason) > 2000 || strings.ContainsRune(reason, 0) {
		return Dispute{}, ErrInvalid
	}
	var result Dispute
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var eventID string
		if err := tx.QueryRow(ctx, `SELECT b.event_id::text FROM bookings b WHERE b.id = $1 AND `+tenantGuard+`
			FOR UPDATE OF b`, bookingID, actor).Scan(&eventID); err != nil {
			return err
		}
		var paid, released bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM ledger_transactions t JOIN payment_attempts a ON a.id = t.ref_id
				WHERE t.kind = 'payment_received' AND t.ref_type = 'payment_attempt' AND a.booking_id = $1),
			EXISTS (SELECT 1 FROM payouts WHERE event_id = $2 AND status = 'released')`, bookingID, eventID).Scan(&paid, &released); err != nil {
			return err
		}
		if !paid {
			return ErrNotConfirmed
		}
		if released {
			return ErrDisputeClosed
		}
		var err error
		result, err = scanDispute(tx.QueryRow(ctx, `INSERT INTO disputes AS d (booking_id, event_id, opened_by, reason)
			VALUES ($1, $2, $3, $4) RETURNING `+disputeColumns, bookingID, eventID, actor, reason))
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23505" {
			return ErrDisputeOpen
		}
		return err
	})
	return result, err
}

// ResolveDispute closes a dispute. A refund outcome owes the tenant money back for part or all of the rent not
// already refunded; a rejection leaves the booking as it is. Either way the payout block ends.
func (s Store) ResolveDispute(ctx context.Context, admin, id, outcome string, amount int64, note string) (Dispute, *Refund, error) {
	note = strings.TrimSpace(note)
	if !httpapi.ValidID(id) || (outcome != "refund" && outcome != "reject") || note == "" || utf8.RuneCountInString(note) > 2000 ||
		(outcome == "refund" && amount < 1) {
		return Dispute{}, nil, ErrInvalid
	}
	var result Dispute
	var refund *Refund
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if err := requireAdmin(ctx, tx, admin); err != nil {
			return err
		}
		d, err := scanDispute(tx.QueryRow(ctx, `SELECT `+disputeColumns+` FROM disputes d WHERE d.id = $1 FOR UPDATE OF d`, id))
		if err != nil {
			return err
		}
		if d.Status != "open" {
			return ErrState
		}
		status := "rejected"
		var refunded *int64
		if outcome == "refund" {
			r, _, err := requestRefund(ctx, tx, d.BookingID, "dispute", amount)
			if err != nil {
				return err
			}
			refund, status, refunded = &r, "refunded", &amount
		}
		result, err = scanDispute(tx.QueryRow(ctx, `UPDATE disputes d SET status = $2, resolution_note = $3, refund_amount = $4,
			resolved_by = $5, resolved_at = clock_timestamp() WHERE d.id = $1 RETURNING `+disputeColumns, id, status, note, refunded, admin))
		if err != nil {
			return err
		}
		return audit(ctx, tx, admin, "dispute.resolve", "dispute", id, note, d, result)
	})
	return result, refund, err
}

// ConfirmRefund is the operator's confirmation that a refund reached the tenant, for refunds the provider cannot send.
func (s Store) ConfirmRefund(ctx context.Context, admin, id, providerRef string, fee int64) (Refund, error) {
	providerRef = strings.TrimSpace(providerRef)
	if !httpapi.ValidID(id) || providerRef == "" || len(providerRef) > 100 || fee < 0 {
		return Refund{}, ErrInvalid
	}
	var result Refund
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if err := requireAdmin(ctx, tx, admin); err != nil {
			return err
		}
		before, err := scanRefund(tx.QueryRow(ctx, `SELECT `+refundColumns+` FROM refunds r WHERE r.id = $1`, id))
		if err != nil {
			return err
		}
		result, err = confirmRefund(ctx, tx, id, providerRef, fee)
		if err != nil {
			return err
		}
		return audit(ctx, tx, admin, "refund.confirm", "refund", id, providerRef, before, result)
	})
	return result, err
}

// FailRefund marks a refund as impossible. The amount stays reserved until someone resolves it.
func (s Store) FailRefund(ctx context.Context, admin, id, reason string) (Refund, error) {
	reason = strings.TrimSpace(reason)
	if !httpapi.ValidID(id) || reason == "" || utf8.RuneCountInString(reason) > 300 {
		return Refund{}, ErrInvalid
	}
	var result Refund
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if err := requireAdmin(ctx, tx, admin); err != nil {
			return err
		}
		before, err := scanRefund(tx.QueryRow(ctx, `SELECT `+refundColumns+` FROM refunds r WHERE r.id = $1 FOR UPDATE OF r`, id))
		if err != nil {
			return err
		}
		if before.Status == "refunded" {
			return ErrState
		}
		result, err = scanRefund(tx.QueryRow(ctx, `UPDATE refunds r SET status = 'failed', failure = $2, updated_at = clock_timestamp()
			WHERE r.id = $1 RETURNING `+refundColumns, id, reason))
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO payment_exceptions (attempt_id, booking_id, kind, detail)
			SELECT a.id, a.booking_id, 'refund_failed', jsonb_build_object('refund_id', $1::text, 'reason', $2::text)
			FROM refunds f JOIN payment_attempts a ON a.id = f.attempt_id WHERE f.id = $1`, id, reason); err != nil {
			return err
		}
		return audit(ctx, tx, admin, "refund.fail", "refund", id, reason, before, result)
	})
	return result, err
}
