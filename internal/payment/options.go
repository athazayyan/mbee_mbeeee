package payment

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// MethodOption is one way to pay a booking with its fee and the exact total.
type MethodOption struct {
	Code        string `json:"code"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	Fee         int64  `json:"fee"`
	Total       int64  `json:"total"`
	FeeSource   string `json:"fee_source"`
	RefundMode  string `json:"refund_mode"`
}

// Options is the checkout summary for a booking.
type Options struct {
	BookingID string         `json:"booking_id"`
	Status    string         `json:"status"`
	ExpiresAt time.Time      `json:"expires_at"`
	Rent      int64          `json:"rent"`
	FeeBearer string         `json:"fee_bearer"`
	Payable   bool           `json:"payable"`
	Methods   []MethodOption `json:"methods"`
	// Attempt is the latest payment attempt, so a payer who returns from the provider can see where it stands.
	Attempt *Attempt `json:"attempt"`
}

// PaymentOptions returns what the tenant can pay with and the total for each method. It reads state only.
func (s Store) PaymentOptions(ctx context.Context, actor, bookingID string) (Options, error) {
	result := Options{BookingID: bookingID, FeeBearer: feeBearer, Methods: []MethodOption{}}
	err := pgx.BeginTxFunc(ctx, s.DB, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var eventStatus string
		var live bool
		err := tx.QueryRow(ctx, `SELECT b.status, b.rent, b.expires_at, e.status, b.expires_at > clock_timestamp()
			FROM bookings b JOIN events e ON e.id = b.event_id
			WHERE b.id = $1 AND (b.created_by = $2 OR EXISTS (
				SELECT 1 FROM memberships m WHERE m.user_id = $2 AND m.role = 'owner' AND m.business_id = b.business_id))`,
			bookingID, actor).Scan(&result.Status, &result.Rent, &result.ExpiresAt, &eventStatus, &live)
		if err != nil {
			return err
		}
		result.Payable = s.Provider != nil && live && result.Rent >= 1 &&
			(result.Status == "held" || result.Status == "payment_pending") &&
			(eventStatus == "published" || eventStatus == "live")
		methods, err := enabledMethods(ctx, tx)
		if err != nil {
			return err
		}
		for _, m := range methods {
			fee, err := m.Fee(result.Rent)
			if err != nil {
				return err
			}
			result.Methods = append(result.Methods, MethodOption{
				Code: m.Code, Label: m.Label, Description: m.Description, Fee: fee, Total: result.Rent + fee,
				FeeSource: m.FeeSource, RefundMode: m.RefundMode,
			})
		}
		latest, err := scanAttempt(tx.QueryRow(ctx, `SELECT `+attemptColumns+` FROM payment_attempts
			WHERE booking_id = $1 ORDER BY created_at DESC, id DESC LIMIT 1`, bookingID))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		result.Attempt = &latest
		return nil
	})
	return result, err
}
