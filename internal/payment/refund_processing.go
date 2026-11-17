package payment

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// ProcessCancellationRefund advances one due refund. The booking ID is a stable
// provider key. Always reconcile first, including after an uncertain response.
func (s Store) ProcessCancellationRefund(ctx context.Context) error {
	if s.Provider == nil {
		return ErrUnavailable
	}
	var providerErr error
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var booking, order, state string
		var amount, original int64
		var retryWindow bool
		err := tx.QueryRow(ctx, `SELECT r.booking_id, r.payment_attempt_id, r.status, r.amount, p.amount,
			r.requested_at > clock_timestamp() - interval '7 days'
			FROM event_cancellation_refunds r JOIN payment_attempts p ON p.id = r.payment_attempt_id
			WHERE r.status IN ('queued', 'submitted') AND r.next_check_at <= clock_timestamp()
			ORDER BY r.next_check_at, r.booking_id LIMIT 1 FOR UPDATE OF r SKIP LOCKED`).
			Scan(&booking, &order, &state, &amount, &original, &retryWindow)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		key := "event-cancel-" + booking
		verified, err := s.Provider.Status(ctx, order)
		listed, matches := refundListed(verified, key, amount)
		switch {
		case err != nil:
			providerErr = err
		case !matchesRefundOrder(verified, original):
			state = "manual_review"
		case verified.confirmsRefund(key, amount):
			return confirmCancellationRefund(ctx, tx, booking, amount)
		case listed && !matches:
			state = "manual_review"
		case listed:
			state = "submitted"
		case verified.PaymentType != "qris" || !retryWindow:
			state = "manual_review"
		case state == "queued" && verified.TransactionStatus == "settlement":
			if err := s.Provider.requestRefund(ctx, order, key, amount); err != nil {
				providerErr = err
			} else {
				state = "submitted"
			}
		case state == "queued":
			state = "manual_review"
		}
		_, err = tx.Exec(ctx, `UPDATE event_cancellation_refunds SET status = $2,
			next_check_at = clock_timestamp() + interval '1 minute' WHERE booking_id = $1`, booking, state)
		return err
	})
	return errors.Join(err, providerErr)
}

func matchesRefundOrder(n Notification, original int64) bool {
	total, ok := wholeRupiah(n.GrossAmount)
	return ok && total == original && n.Currency == "IDR"
}

func refundListed(n Notification, key string, amount int64) (bool, bool) {
	for _, refund := range n.Refunds {
		if refund.Key == key {
			listed, ok := wholeRupiah(refund.Amount)
			return true, ok && listed == amount
		}
	}
	return false, false
}

func confirmCancellationRefund(ctx context.Context, tx pgx.Tx, booking string, amount int64) error {
	var transaction string
	if err := tx.QueryRow(ctx, `INSERT INTO ledger_transactions (kind, ref_type, ref_id)
		VALUES ('event_cancellation_refund', 'booking', $1) RETURNING id`, booking).Scan(&transaction); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger_entries (transaction_id, account, direction, amount) VALUES
		($1, 'organizer_escrow', 'debit', $2), ($1, 'provider_clearing', 'credit', $2)`, transaction, amount); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE event_cancellation_refunds SET status = 'confirmed',
		confirmed_at = clock_timestamp() WHERE booking_id = $1`, booking)
	return err
}
