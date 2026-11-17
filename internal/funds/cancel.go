package funds

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

// Settlement summarizes what an event cancellation did to its bookings.
type Settlement struct {
	ClosedHolds   int   `json:"closed_holds"`
	ClosedPending int   `json:"closed_pending_payments"`
	Refunds       int   `json:"refunds"`
	RefundTotal   int64 `json:"refund_total"`
}

// SettleCancellation closes every live booking of a cancelled event in the caller's transaction (E09-05).
//
//   - Holds and curated offers are cancelled and their stands released.
//   - Bookings waiting for payment are cancelled, their payment attempts are closed here, and the provider
//     transactions are queued to be closed right after commit. A payment that still arrives is queued as a
//     late_payment exception by the payment webhook.
//   - Confirmed bookings are cancelled, released, and owed a refund of RefundPercent of the rent snapshot,
//     reserved from the organizer's escrow. The admin fee paid at checkout is not refunded.
//   - Applications still waiting for a decision are withdrawn.
//
// Every cancelled booking gets a status-history row with reason event_cancelled, which is what the
// notification worker turns into a message for the tenant. Calling it again changes nothing.
func SettleCancellation(ctx context.Context, tx pgx.Tx, eventID, actor string) (Settlement, error) {
	var out Settlement
	if !httpapi.ValidID(eventID) {
		return out, ErrNotFound
	}
	type live struct {
		id, status string
		rent       int64
	}
	rows, err := tx.Query(ctx, `SELECT id::text, status, rent FROM bookings
		WHERE event_id = $1 AND status IN ('held', 'payment_pending', 'confirmed') ORDER BY id FOR UPDATE`, eventID)
	if err != nil {
		return out, err
	}
	bookings, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (live, error) {
		var b live
		err := row.Scan(&b.id, &b.status, &b.rent)
		return b, err
	})
	if err != nil {
		return out, err
	}
	for _, b := range bookings {
		if _, err := tx.Exec(ctx, `UPDATE bookings SET status = 'cancelled' WHERE id = $1`, b.id); err != nil {
			return out, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO booking_status_history (booking_id, actor_id, from_status, to_status, reason)
			VALUES ($1, $2, $3, 'cancelled', 'event_cancelled')`, b.id, actor, b.status); err != nil {
			return out, err
		}
		if _, err := tx.Exec(ctx, `UPDATE allocations SET status = 'released'
			WHERE booking_id = $1 AND status IN ('hold', 'offer', 'reserved')`, b.id); err != nil {
			return out, err
		}
		switch b.status {
		case "held":
			out.ClosedHolds++
		case "payment_pending":
			out.ClosedPending++
			if _, err := tx.Exec(ctx, `INSERT INTO payment_closures (attempt_id)
				SELECT id FROM payment_attempts WHERE booking_id = $1 AND status IN ('created', 'pending')
				ON CONFLICT DO NOTHING`, b.id); err != nil {
				return out, err
			}
			if _, err := tx.Exec(ctx, `UPDATE payment_attempts SET status = 'expired', updated_at = clock_timestamp()
				WHERE booking_id = $1 AND status IN ('created', 'pending')`, b.id); err != nil {
				return out, err
			}
		case "confirmed":
			amount := CancellationRefund(b.rent)
			if amount < 1 {
				continue // nothing to give back on a free or one-rupiah rent
			}
			refund, created, err := requestRefund(ctx, tx, b.id, "event_cancelled", amount)
			if errors.Is(err, pgx.ErrNoRows) {
				// Confirmed without a recorded payment: do not block the cancellation, ask a person to look.
				if _, err := tx.Exec(ctx, `INSERT INTO payment_exceptions (booking_id, kind, detail)
					VALUES ($1, 'manual_refund_required', jsonb_build_object('reason', 'confirmed booking has no paid attempt'))`, b.id); err != nil {
					return out, err
				}
				continue
			}
			if err != nil {
				return out, err
			}
			if created {
				out.Refunds++
				out.RefundTotal += refund.Amount
			}
		}
	}
	// Applications waiting for a decision can no longer be approved. Approved ones follow their booking.
	if _, err := tx.Exec(ctx, `INSERT INTO application_status_history (application_id, actor_id, from_status, to_status, reason)
		SELECT id, $2, 'submitted', 'withdrawn', 'event_cancelled' FROM applications
		WHERE event_id = $1 AND status = 'submitted'`, eventID, actor); err != nil {
		return out, err
	}
	_, err = tx.Exec(ctx, `UPDATE applications SET status = 'withdrawn', updated_at = clock_timestamp()
		WHERE event_id = $1 AND status = 'submitted'`, eventID)
	return out, err
}

// CancellationPreview shows an organizer what cancelling would do, before they confirm.
type CancellationPreview struct {
	Holds         int   `json:"holds"`
	PendingPay    int   `json:"pending_payments"`
	Confirmed     int   `json:"confirmed"`
	RefundTotal   int64 `json:"refund_total"`
	RefundPercent int   `json:"refund_percent"`
}

// PreviewCancellation counts what SettleCancellation would touch. It changes nothing.
func (s Store) PreviewCancellation(ctx context.Context, actor, eventID string) (CancellationPreview, error) {
	out := CancellationPreview{RefundPercent: RefundPercent}
	if !httpapi.ValidID(actor) || !httpapi.ValidID(eventID) {
		return out, ErrNotFound
	}
	err := pgx.BeginTxFunc(ctx, s.DB, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		owner, err := isOwner(ctx, tx, actor, eventID)
		if err != nil {
			return err
		}
		if !owner {
			return ErrNotFound
		}
		return tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status = 'held'),
				count(*) FILTER (WHERE status = 'payment_pending'), count(*) FILTER (WHERE status = 'confirmed'),
				COALESCE(SUM(rent * $2 / 100) FILTER (WHERE status = 'confirmed'), 0)::bigint
			FROM bookings WHERE event_id = $1`, eventID, RefundPercent).
			Scan(&out.Holds, &out.PendingPay, &out.Confirmed, &out.RefundTotal)
	})
	return out, err
}
