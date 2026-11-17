package payment

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

// ReconcileCancelledPayment reads one open attempt for a cancelled booking.
// Webhooks and polling share the same deduplication and settlement rules.
func (s Store) ReconcileCancelledPayment(ctx context.Context) error {
	if s.Provider == nil {
		return ErrUnavailable
	}
	var orderID string
	err := s.DB.QueryRow(ctx, `WITH due AS (
		SELECT p.id FROM payment_attempts p JOIN bookings b ON b.id = p.booking_id
		WHERE b.status = 'cancelled' AND p.status IN ('created', 'pending')
		  AND (p.cancellation_checked_at IS NULL OR p.cancellation_checked_at < clock_timestamp() - interval '1 minute')
		ORDER BY p.cancellation_checked_at NULLS FIRST, p.id
		LIMIT 1 FOR UPDATE OF p SKIP LOCKED
	)
	UPDATE payment_attempts p SET cancellation_checked_at = clock_timestamp()
	FROM due WHERE p.id = due.id RETURNING p.id`).Scan(&orderID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	verified, err := s.Provider.Status(ctx, orderID)
	if err != nil {
		return err
	}
	status, known := verified.outcome()
	if !known || status == "pending" {
		return nil
	}
	payload, err := json.Marshal(verified)
	if err != nil {
		return err
	}
	return s.ApplyNotification(ctx, verified, payload)
}
