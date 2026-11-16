package booking

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	DB *pgxpool.Pool
}

type Price struct {
	Currency    string `json:"currency"`
	Rent        int64  `json:"rent"`
	PlatformFee int64  `json:"platform_fee"`
	ProviderFee int64  `json:"provider_fee"`
	Total       int64  `json:"total"`
	Version     string `json:"price_version"`
}

type Booking struct {
	ID            string    `json:"booking_id"`
	BusinessID    string    `json:"business_id"`
	EventID       string    `json:"event_id"`
	EventName     string    `json:"event_name"`
	StandID       string    `json:"stand_id"`
	SlotID        string    `json:"event_slot_id"`
	Status        string    `json:"status"`
	ExpiresAt     time.Time `json:"expires_at"`
	CreatedAt     time.Time `json:"created_at"`
	PolicyVersion string    `json:"accepted_policy_version"`
	Price         Price     `json:"price"`
	ServerNow     time.Time `json:"-"`
}

const bookingColumns = `b.id, b.business_id, b.event_id, e.name, a.stand_id, a.event_slot_id,
	CASE WHEN b.status IN ('held', 'payment_pending') AND b.expires_at <= transaction_timestamp()
		THEN 'expired' ELSE b.status END,
	b.expires_at, b.created_at, b.refund_policy_version, b.rent, b.platform_fee, b.provider_fee,
	b.total, b.price_revision::text, transaction_timestamp()`

func (s Store) Ready(ctx context.Context) error {
	_, err := s.DB.Exec(ctx, `SELECT 1 FROM bookings, allocations, booking_status_history LIMIT 0`)
	return err
}

func (s Store) detail(ctx context.Context, actor, id string) (Booking, error) {
	row := s.DB.QueryRow(ctx, `SELECT `+bookingColumns+` FROM bookings b
		JOIN allocations a ON a.booking_id = b.id
		JOIN events e ON e.id = b.event_id
		WHERE b.id = $1 AND EXISTS (
			SELECT 1 FROM memberships m WHERE m.user_id = $2 AND m.role = 'owner'
			AND (m.business_id = b.business_id OR m.organization_id = e.organization_id)
		)`, id, actor)
	return scanBooking(row)
}

func (s Store) list(ctx context.Context, actor, businessID string, before time.Time, beforeID *string, limit int) ([]Booking, time.Time, error) {
	bookings := make([]Booking, 0)
	var serverNow time.Time
	err := pgx.BeginTxFunc(ctx, s.DB, pgx.TxOptions{
		IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly,
	}, func(tx pgx.Tx) error {
		var business string
		if err := tx.QueryRow(ctx, `SELECT business_id, transaction_timestamp() FROM memberships
			WHERE user_id = $1 AND business_id = $2 AND role = 'owner'`, actor, businessID).Scan(&business, &serverNow); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+bookingColumns+` FROM bookings b
			JOIN allocations a ON a.booking_id = b.id JOIN events e ON e.id = b.event_id
			WHERE b.business_id = $1 AND ($2::uuid IS NULL OR (b.created_at, b.id) < ($3, $2))
			ORDER BY b.created_at DESC, b.id DESC LIMIT $4`, business, beforeID, before, limit)
		if err != nil {
			return err
		}
		bookings, err = pgx.AppendRows(bookings, rows, func(row pgx.CollectableRow) (Booking, error) {
			return scanBooking(row)
		})
		return err
	})
	return bookings, serverNow, err
}

func scanBooking(row pgx.Row) (Booking, error) {
	var result Booking
	result.Price.Currency = "IDR"
	err := row.Scan(&result.ID, &result.BusinessID, &result.EventID, &result.EventName, &result.StandID, &result.SlotID, &result.Status,
		&result.ExpiresAt, &result.CreatedAt, &result.PolicyVersion, &result.Price.Rent, &result.Price.PlatformFee,
		&result.Price.ProviderFee, &result.Price.Total, &result.Price.Version, &result.ServerNow)
	return result, err
}

func (s Store) Expire(ctx context.Context, limit int) (int64, error) {
	if limit < 1 {
		return 0, errors.New("expiry batch size must be positive")
	}
	return expireHolds(ctx, s.DB, nil, &limit)
}

type expiryExecutor interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

func expireHolds(ctx context.Context, executor expiryExecutor, eventID *string, limit *int) (int64, error) {
	result, err := executor.Exec(ctx, `
		WITH due AS (
			SELECT b.id, b.status FROM bookings b
			JOIN allocations a ON a.booking_id = b.id AND a.status IN ('hold', 'offer')
			WHERE b.status IN ('held', 'payment_pending') AND b.expires_at <= clock_timestamp()
				AND ($1::uuid IS NULL OR b.event_id = $1)
			ORDER BY b.expires_at, b.id LIMIT $2 FOR UPDATE OF b SKIP LOCKED
		), released AS (
			UPDATE allocations a SET status = 'released' FROM due
			WHERE a.booking_id = due.id AND a.status IN ('hold', 'offer') RETURNING a.booking_id
		), expired AS (
			UPDATE bookings b SET status = 'expired' FROM due
			JOIN released a ON a.booking_id = due.id WHERE b.id = due.id
			RETURNING b.id, due.status
		)
		INSERT INTO booking_status_history (booking_id, from_status, to_status, reason)
		SELECT id, status, 'expired', 'deadline_elapsed' FROM expired
	`, eventID, limit)
	return result.RowsAffected(), err
}
