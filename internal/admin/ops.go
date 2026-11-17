package admin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"kitakarsa.com/tenantku/backend/internal/platform/adminperm"
)

var errUnavailable = errors.New("payment provider is not configured")

// BookingRow is one booking in the admin search, with its latest payment attempt.
type BookingRow struct {
	ID            string    `json:"id"`
	Status        string    `json:"status"`
	EventID       string    `json:"event_id"`
	EventName     string    `json:"event_name"`
	BusinessID    string    `json:"business_id"`
	BusinessName  string    `json:"business_name"`
	Rent          int64     `json:"rent"`
	CreatedAt     time.Time `json:"created_at"`
	PaymentStatus *string   `json:"payment_status,omitempty"`
	PaymentAmount *int64    `json:"payment_amount,omitempty"`
}

// searchTerm lowercases the query so UUIDs compare in their canonical text form.
func searchTerm(filter pageFilter) string { return strings.ToLower(filter.Query) }

func (s Store) bookings(ctx context.Context, filter pageFilter) ([]BookingRow, error) {
	rows, err := s.DB.Query(ctx, `SELECT b.id::text, b.status, b.event_id::text, e.name, b.business_id::text, z.name, b.rent, b.created_at,
			pa.status, pa.amount
		FROM bookings b JOIN events e ON e.id = b.event_id JOIN businesses z ON z.id = b.business_id
		LEFT JOIN LATERAL (SELECT p.status, p.amount FROM payment_attempts p WHERE p.booking_id = b.id
			ORDER BY p.created_at DESC LIMIT 1) pa ON true
		WHERE ($1 = '' OR b.status = $1)
		AND ($2 = '' OR b.id::text = $2 OR b.event_id::text = $2 OR b.business_id::text = $2
			OR strpos(lower(e.name), $2) > 0 OR strpos(lower(z.name), $2) > 0)
		AND ($3::uuid IS NULL OR (b.created_at, b.id) < ($4, $3::uuid))
		ORDER BY b.created_at DESC, b.id DESC LIMIT $5`,
		filter.Status, searchTerm(filter), nilIfEmpty(filter.AfterID), filter.AfterAt, filter.Limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[BookingRow])
}

// PaymentRow is one payment attempt, for a booking or for an event plan purchase.
type PaymentRow struct {
	ID            string     `json:"id"`
	Kind          string     `json:"kind"`
	Status        string     `json:"status"`
	Method        string     `json:"method"`
	Amount        int64      `json:"amount"`
	ProviderFee   int64      `json:"provider_fee"`
	FeeVersion    *string    `json:"fee_version,omitempty"`
	BookingID     *string    `json:"booking_id,omitempty"`
	EventName     *string    `json:"event_name,omitempty"`
	BusinessName  *string    `json:"business_name,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	PaidAt        *time.Time `json:"paid_at,omitempty"`
	OpenException int64      `json:"open_exceptions"`
}

func (s Store) payments(ctx context.Context, filter pageFilter) ([]PaymentRow, error) {
	rows, err := s.DB.Query(ctx, `SELECT p.id::text, CASE WHEN p.purchase_id IS NULL THEN 'booking' ELSE 'plan' END,
			p.status, p.method, p.amount, COALESCE(p.provider_fee_amount, 0), p.fee_version, p.booking_id::text, e.name, z.name, p.created_at, p.paid_at,
			(SELECT count(*) FROM payment_exceptions x WHERE x.attempt_id = p.id AND x.status = 'open')
		FROM payment_attempts p LEFT JOIN bookings b ON b.id = p.booking_id
		LEFT JOIN events e ON e.id = b.event_id LEFT JOIN businesses z ON z.id = b.business_id
		WHERE ($1 = '' OR p.status = $1)
		AND ($2 = '' OR p.id::text = $2 OR p.booking_id::text = $2 OR strpos(lower(COALESCE(e.name, '')), $2) > 0
			OR strpos(lower(COALESCE(z.name, '')), $2) > 0)
		AND ($3::uuid IS NULL OR (p.created_at, p.id) < ($4, $3::uuid))
		ORDER BY p.created_at DESC, p.id DESC LIMIT $5`,
		filter.Status, searchTerm(filter), nilIfEmpty(filter.AfterID), filter.AfterAt, filter.Limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[PaymentRow])
}

// ExceptionRow is a payment exception waiting for, or closed by, a person.
type ExceptionRow struct {
	ID             string          `json:"id"`
	Kind           string          `json:"kind"`
	Status         string          `json:"status"`
	AttemptID      *string         `json:"attempt_id,omitempty"`
	BookingID      *string         `json:"booking_id,omitempty"`
	Detail         json.RawMessage `json:"detail"`
	CreatedAt      time.Time       `json:"created_at"`
	ResolvedAt     *time.Time      `json:"resolved_at,omitempty"`
	ResolutionNote *string         `json:"resolution_note,omitempty"`
}

const exceptionColumns = `x.id::text, x.kind, x.status, x.attempt_id::text, x.booking_id::text, x.detail, x.created_at,
	x.resolved_at, x.resolution_note`

func (s Store) exceptions(ctx context.Context, filter pageFilter) ([]ExceptionRow, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+exceptionColumns+` FROM payment_exceptions x
		WHERE ($1 = '' OR x.status = $1)
		AND ($2::uuid IS NULL OR (x.created_at, x.id) < ($3, $2::uuid))
		ORDER BY x.created_at DESC, x.id DESC LIMIT $4`,
		filter.Status, nilIfEmpty(filter.AfterID), filter.AfterAt, filter.Limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[ExceptionRow])
}

// resolveException closes an exception with a note. Closing one that is already closed returns it unchanged,
// so a retry writes neither a second change nor a second audit row.
func (s Store) resolveException(ctx context.Context, actor, id, note, requestID string) (ExceptionRow, error) {
	var result ExceptionRow
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if err := requireAdmin(ctx, tx, actor, adminperm.PaymentsManage); err != nil {
			return err
		}
		before, err := scanException(tx.QueryRow(ctx, `SELECT `+exceptionColumns+` FROM payment_exceptions x WHERE x.id = $1 FOR UPDATE`, id))
		if err != nil {
			return err
		}
		result = before
		if before.Status == "resolved" {
			return nil
		}
		result, err = scanException(tx.QueryRow(ctx, `WITH u AS (UPDATE payment_exceptions x SET status = 'resolved',
				resolved_by = $2, resolved_at = clock_timestamp(), resolution_note = $3 WHERE x.id = $1 RETURNING x.*)
			SELECT `+strings.ReplaceAll(exceptionColumns, "x.", "u.")+` FROM u`, id, actor, note))
		if err != nil {
			return err
		}
		return writeAudit(ctx, tx, auditRecord{actor: actor, action: "payment_exception.resolve", resourceType: "payment_exception",
			resourceID: id, reason: note, requestID: requestID,
			before: map[string]any{"status": before.Status, "kind": before.Kind}, after: map[string]any{"status": result.Status}})
	})
	return result, err
}

// recheckPayment asks the provider for the real status of an attempt. The provider state is applied through the
// webhook path, which is idempotent, so retrying never pays or refunds twice.
func (s Store) recheckPayment(ctx context.Context, actor, id, reason, requestID string) (PaymentRow, error) {
	if s.Payments == nil {
		return PaymentRow{}, errUnavailable
	}
	var before string
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if err := requireAdmin(ctx, tx, actor, adminperm.PaymentsManage); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT status FROM payment_attempts WHERE id = $1`, id).Scan(&before)
	})
	if err != nil {
		return PaymentRow{}, err
	}
	if err := s.Payments.Recheck(ctx, id); err != nil {
		return PaymentRow{}, err
	}
	var result PaymentRow
	err = pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		rows, err := s.payments(ctx, pageFilter{Query: id, Limit: 1})
		if err != nil || len(rows) == 0 {
			return errors.Join(err, pgx.ErrNoRows)
		}
		result = rows[0]
		return writeAudit(ctx, tx, auditRecord{actor: actor, action: "payment.recheck", resourceType: "payment_attempt",
			resourceID: id, reason: reason, requestID: requestID,
			before: map[string]string{"status": before}, after: map[string]string{"status": result.Status}})
	})
	return result, err
}

func scanException(row pgx.Row) (ExceptionRow, error) {
	var x ExceptionRow
	err := row.Scan(&x.ID, &x.Kind, &x.Status, &x.AttemptID, &x.BookingID, &x.Detail, &x.CreatedAt, &x.ResolvedAt, &x.ResolutionNote)
	return x, err
}

// EventRow is one event in the admin directory.
type EventRow struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	Status           string    `json:"status"`
	City             string    `json:"city"`
	OrganizationName string    `json:"organization_name"`
	StartsAt         time.Time `json:"starts_at"`
	CreatedAt        time.Time `json:"created_at"`
	Bookings         int64     `json:"bookings"`
}

func (s Store) events(ctx context.Context, filter pageFilter) ([]EventRow, error) {
	rows, err := s.DB.Query(ctx, `SELECT e.id::text, e.name, e.status, e.city, o.name, e.starts_at, e.created_at,
			(SELECT count(*) FROM bookings b WHERE b.event_id = e.id)
		FROM events e JOIN organizations o ON o.id = e.organization_id
		WHERE ($1 = '' OR e.status = $1)
		AND ($2 = '' OR e.id::text = $2 OR strpos(lower(e.name), $2) > 0 OR strpos(lower(o.name), $2) > 0
			OR strpos(lower(e.city), $2) > 0)
		AND ($3::uuid IS NULL OR (e.created_at, e.id) < ($4, $3::uuid))
		ORDER BY e.created_at DESC, e.id DESC LIMIT $5`,
		filter.Status, searchTerm(filter), nilIfEmpty(filter.AfterID), filter.AfterAt, filter.Limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[EventRow])
}
