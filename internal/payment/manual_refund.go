package payment

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

// RecordManualVARefund records a completed external transfer. It never sends money.
func (s Store) RecordManualVARefund(ctx context.Context, booking string, amount int64, reference string) error {
	reference = strings.ToUpper(strings.TrimSpace(reference))
	if !httpapi.ValidID(booking) || !httpapi.ValidSingleLine(reference, 128) || amount < 1 {
		return errors.New("invalid booking ID, refund amount or bank reference")
	}
	if s.Provider == nil {
		return ErrUnavailable
	}
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var state, order string
		var expected, gross int64
		err := tx.QueryRow(ctx, `SELECT r.status, r.amount, p.id, p.amount
			FROM event_cancellation_refunds r JOIN payment_attempts p ON p.id = r.payment_attempt_id
			WHERE r.booking_id = $1 AND p.method = 'bca_va' AND p.status = 'paid' FOR UPDATE OF r`, booking).
			Scan(&state, &expected, &order, &gross)
		if err != nil {
			return err
		}
		if amount != expected {
			return errors.New("transfer amount does not match the refund instruction")
		}
		if state == "confirmed" {
			var existing string
			err := tx.QueryRow(ctx, `SELECT bank_reference FROM manual_event_refund_receipts WHERE booking_id = $1`, booking).Scan(&existing)
			if err == nil && existing == reference {
				return nil
			}
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		if state != "manual_review" {
			return errors.New("refund is not awaiting manual VA confirmation")
		}
		verified, err := s.Provider.Status(ctx, order)
		if err != nil {
			return err
		}
		if !matchesRefundOrder(verified, gross) || verified.PaymentType != "bank_transfer" ||
			verified.TransactionStatus != "settlement" {
			return errors.New("provider has not verified the original VA settlement")
		}
		if _, err := tx.Exec(ctx, `INSERT INTO manual_event_refund_receipts (booking_id, bank_reference)
			VALUES ($1, $2)`, booking, reference); err != nil {
			return err
		}
		return confirmCancellationRefund(ctx, tx, booking, amount, "manual_refund_cash")
	})
}
