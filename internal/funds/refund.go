package funds

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"kitakarsa.com/tenantku/backend/internal/payment"
)

// Refund is money owed back to a tenant.
type Refund struct {
	ID          string     `json:"id"`
	BookingID   string     `json:"booking_id"`
	EventID     string     `json:"event_id"`
	Reason      string     `json:"reason"`
	Amount      int64      `json:"amount"`
	RentAmount  int64      `json:"rent_amount"`
	Fee         int64      `json:"fee"`
	Mode        string     `json:"mode"`
	Status      string     `json:"status"`
	ProviderRef *string    `json:"provider_ref,omitempty"`
	Failure     *string    `json:"failure,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	RefundedAt  *time.Time `json:"refunded_at,omitempty"`
}

const refundColumns = `r.id::text, r.booking_id::text, r.event_id::text, r.reason, r.amount, r.rent_amount, r.fee_amount,
	r.mode, r.status, r.provider_ref, r.failure, r.created_at, r.refunded_at`

// allRefunds lists dispute refunds together with the refunds of cancelled events, which the payment package
// owns (event_cancellation_refunds). Cancellation refunds are shown in the funds-module shape and read-only.
const allRefunds = `(SELECT id, booking_id, event_id, reason, amount, rent_amount, fee_amount, mode, status,
		provider_ref, failure, created_at, refunded_at FROM refunds
	UNION ALL
	SELECT c.booking_id, c.booking_id, b.event_id, 'event_cancelled'::text, c.amount, b.rent, 0::bigint,
		CASE WHEN c.status = 'manual_review' THEN 'manual' ELSE 'api' END,
		CASE c.status WHEN 'confirmed' THEN 'refunded' WHEN 'manual_review' THEN 'manual_required' ELSE 'requested' END,
		NULL::text, NULL::text, c.requested_at, c.confirmed_at
	FROM event_cancellation_refunds c JOIN bookings b ON b.id = c.booking_id) r`

func scanRefund(row pgx.Row) (Refund, error) {
	var r Refund
	err := row.Scan(&r.ID, &r.BookingID, &r.EventID, &r.Reason, &r.Amount, &r.RentAmount, &r.Fee, &r.Mode, &r.Status,
		&r.ProviderRef, &r.Failure, &r.CreatedAt, &r.RefundedAt)
	return r, err
}

// requestRefund owes a tenant money back and sets it aside from the organizer's escrow, in the caller's transaction.
// The refund is tied to the booking's paid attempt, and the total refunded for a booking never exceeds its rent.
// It is idempotent per (booking, reason).
func requestRefund(ctx context.Context, tx pgx.Tx, bookingID, reason string, amount int64) (Refund, bool, error) {
	var attemptID, method, eventID string
	var rent int64
	err := tx.QueryRow(ctx, `SELECT a.id::text, a.method, b.event_id::text, COALESCE(a.rent_amount, b.rent)
		FROM payment_attempts a JOIN bookings b ON b.id = a.booking_id
		WHERE a.booking_id = $1 AND a.status = 'paid'
			AND EXISTS (SELECT 1 FROM ledger_transactions t WHERE t.kind = 'payment_received'
				AND t.ref_type = 'payment_attempt' AND t.ref_id = a.id)
		ORDER BY a.paid_at DESC LIMIT 1`, bookingID).Scan(&attemptID, &method, &eventID, &rent)
	if err != nil {
		return Refund{}, false, err
	}
	var refunded int64
	if err := tx.QueryRow(ctx, `SELECT
			COALESCE((SELECT SUM(amount) FROM refunds WHERE booking_id = $1 AND status <> 'failed'), 0)
			+ COALESCE((SELECT amount FROM event_cancellation_refunds WHERE booking_id = $1), 0)`, bookingID).Scan(&refunded); err != nil {
		return Refund{}, false, err
	}
	existing, err := scanRefund(tx.QueryRow(ctx, `SELECT `+refundColumns+` FROM refunds r WHERE r.booking_id = $1 AND r.reason = $2`, bookingID, reason))
	if err == nil {
		return existing, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Refund{}, false, err
	}
	if amount < 1 || refunded+amount > rent {
		return Refund{}, false, ErrAmountTooHigh
	}
	mode := "manual"
	if method != "snap" {
		var configured string
		if err := tx.QueryRow(ctx, `SELECT refund_mode FROM payment_methods WHERE code = $1`, method).Scan(&configured); err == nil {
			mode = configured
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return Refund{}, false, err
		}
	}
	created, err := scanRefund(tx.QueryRow(ctx, `INSERT INTO refunds AS r (booking_id, event_id, attempt_id, reason, amount, rent_amount, mode, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'requested') RETURNING `+refundColumns,
		bookingID, eventID, attemptID, reason, amount, rent, mode))
	if err != nil {
		return Refund{}, false, err
	}
	return created, true, payment.ReserveRefund(ctx, tx, created.ID, amount)
}

// confirmRefund books a refund as paid out. It is safe to call twice.
func confirmRefund(ctx context.Context, tx pgx.Tx, id, providerRef string, fee int64) (Refund, error) {
	r, err := scanRefund(tx.QueryRow(ctx, `SELECT `+refundColumns+` FROM refunds r WHERE r.id = $1 FOR UPDATE OF r`, id))
	if err != nil {
		return Refund{}, err
	}
	if r.Status == "refunded" {
		return r, nil
	}
	if r.Status == "failed" || fee < 0 {
		return Refund{}, ErrState
	}
	if err := payment.RecordRefund(ctx, tx, r.ID, r.Amount); err != nil {
		return Refund{}, err
	}
	if err := payment.RecordRefundFee(ctx, tx, r.ID, fee); err != nil {
		return Refund{}, err
	}
	r, err = scanRefund(tx.QueryRow(ctx, `UPDATE refunds r SET status = 'refunded', refunded_at = clock_timestamp(),
		updated_at = clock_timestamp(), provider_ref = NULLIF($2, ''), fee_amount = $3, failure = NULL
		WHERE r.id = $1 RETURNING `+refundColumns, id, providerRef, fee))
	if err != nil {
		return Refund{}, err
	}
	_, err = tx.Exec(ctx, `UPDATE payment_exceptions SET status = 'resolved'
		WHERE kind = 'manual_refund_required' AND status = 'open' AND detail->>'refund_id' = $1`, id)
	return r, err
}
