package funds

import (
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

// The alphabet leaves out I and O so a code read aloud or from a screenshot is not confused with 1 and 0.
const codeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// QRPrefix marks the QR payload so a scanner can tell a TenantKu code from any other QR.
const QRPrefix = "tenantku:checkin:"

func newCode() (string, error) {
	var raw [10]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	out := make([]byte, len(raw))
	for i, b := range raw {
		out[i] = codeAlphabet[int(b)%len(codeAlphabet)]
	}
	return string(out), nil
}

// NormalizeCode accepts what a person types or a scanner reads: any case, spaces or dashes, or the QR payload.
func NormalizeCode(input string) (string, bool) {
	value := strings.ToUpper(strings.TrimSpace(input))
	value = strings.TrimPrefix(value, strings.ToUpper(QRPrefix))
	value = strings.NewReplacer(" ", "", "-", "").Replace(value)
	if len(value) != 10 {
		return "", false
	}
	for _, c := range value {
		if !strings.ContainsRune(codeAlphabet, c) {
			return "", false
		}
	}
	return value, true
}

// CheckInCode is what a tenant shows at the gate.
type CheckInCode struct {
	Code        string     `json:"code"`
	QR          string     `json:"qr"`
	CheckedInAt *time.Time `json:"checked_in_at"`
}

// CheckInCodeFor returns the booking's code, creating it on first use. Only the tenant who owns a confirmed booking gets one.
func (s Store) CheckInCodeFor(ctx context.Context, actor, bookingID string) (CheckInCode, error) {
	if !httpapi.ValidID(actor) || !httpapi.ValidID(bookingID) {
		return CheckInCode{}, ErrNotFound
	}
	var result CheckInCode
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var status string
		if err := tx.QueryRow(ctx, `SELECT b.status FROM bookings b WHERE b.id = $1 AND `+tenantGuard, bookingID, actor).Scan(&status); err != nil {
			return err
		}
		if status != "confirmed" {
			return ErrNotConfirmed
		}
		for range 5 {
			code, err := newCode()
			if err != nil {
				return err
			}
			var sp = "code"
			if _, err := tx.Exec(ctx, "SAVEPOINT "+sp); err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `INSERT INTO booking_codes (booking_id, code) VALUES ($1, $2) ON CONFLICT (booking_id) DO NOTHING`, bookingID, code)
			var pg *pgconn.PgError
			if errors.As(err, &pg) && pg.Code == "23505" {
				if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT "+sp); err != nil {
					return err
				}
				continue // the random code collided with another booking's
			}
			if err != nil {
				return err
			}
			break
		}
		if err := tx.QueryRow(ctx, `SELECT c.code, i.created_at FROM booking_codes c
			LEFT JOIN check_ins i ON i.booking_id = c.booking_id WHERE c.booking_id = $1`, bookingID).
			Scan(&result.Code, &result.CheckedInAt); err != nil {
			return err
		}
		result.QR = QRPrefix + result.Code
		return nil
	})
	return result, err
}

// tenantGuard limits bookings to the tenant who made them or an owner of their business. It expects $2 to be the actor.
const tenantGuard = `(b.created_by = $2 OR EXISTS (SELECT 1 FROM memberships m
	WHERE m.user_id = $2 AND m.role = 'owner' AND m.business_id = b.business_id))`

// CheckInInput identifies the booking by its code (scan or typed) or, from the staff list, by booking ID.
type CheckInInput struct {
	Code      string `json:"code"`
	BookingID string `json:"booking_id"`
	Method    string `json:"method"`
}

// CheckInResult is what staff see after a check-in.
type CheckInResult struct {
	BookingID    string    `json:"booking_id"`
	BusinessName string    `json:"business_name"`
	StandCode    string    `json:"stand_code,omitempty"`
	CheckedInAt  time.Time `json:"checked_in_at"`
	Already      bool      `json:"already"`
}

// WindowError tells staff when check-in opens and closes.
type WindowError struct{ Opens, Closes time.Time }

func (WindowError) Error() string { return ErrOutsideWindow.Error() }
func (WindowError) Unwrap() error { return ErrOutsideWindow }

// CheckIn marks a guest as arrived. It is idempotent: a second scan returns the first record with Already set.
// Only staff of the event's organization may check in, the booking must be confirmed for this event, and
// the event must be inside its check-in period.
func (s Store) CheckIn(ctx context.Context, actor, eventID string, input CheckInInput) (CheckInResult, error) {
	var result CheckInResult
	if !httpapi.ValidID(actor) || !httpapi.ValidID(eventID) {
		return result, ErrNotFound
	}
	input.BookingID = strings.ToLower(input.BookingID)
	code, hasCode := NormalizeCode(input.Code)
	if (input.Code == "") == (input.BookingID == "") || (input.Code != "" && !hasCode) ||
		(input.BookingID != "" && !httpapi.ValidID(input.BookingID)) {
		return result, ErrInvalid
	}
	method := "manual"
	if input.Method == "qr" && hasCode {
		method = "qr"
	}
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		staff, err := isStaff(ctx, tx, actor, eventID)
		if err != nil {
			return err
		}
		if !staff {
			return ErrNotFound
		}
		var bookingID, status, business, eventStatus string
		var starts, ends, now time.Time
		query := `SELECT b.id, b.status, z.name, e.status, e.starts_at, e.ends_at, clock_timestamp()
			FROM bookings b JOIN events e ON e.id = b.event_id JOIN businesses z ON z.id = b.business_id `
		args := []any{eventID}
		if hasCode {
			query += `JOIN booking_codes c ON c.booking_id = b.id WHERE b.event_id = $1 AND c.code = $2`
			args = append(args, code)
		} else {
			query += `WHERE b.event_id = $1 AND b.id = $2`
			args = append(args, input.BookingID)
		}
		if err := tx.QueryRow(ctx, query, args...).Scan(&bookingID, &status, &business, &eventStatus, &starts, &ends, &now); err != nil {
			return err
		}
		result = CheckInResult{BookingID: bookingID, BusinessName: business}
		_ = tx.QueryRow(ctx, `SELECT s.code FROM allocations a JOIN stands s ON s.id = a.stand_id WHERE a.booking_id = $1`, bookingID).Scan(&result.StandCode)
		// A repeat scan answers with the original record, whatever the clock says now.
		var prior time.Time
		err = tx.QueryRow(ctx, `SELECT created_at FROM check_ins WHERE booking_id = $1`, bookingID).Scan(&prior)
		if err == nil {
			result.CheckedInAt, result.Already = prior, true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if status != "confirmed" || (eventStatus != "published" && eventStatus != "live" && eventStatus != "completed") {
			return ErrNotConfirmed
		}
		opens := starts.Add(-CheckInLead)
		if now.Before(opens) || now.After(ends) {
			return WindowError{Opens: opens, Closes: ends}
		}
		return tx.QueryRow(ctx, `INSERT INTO check_ins (booking_id, event_id, checked_in_by, method) VALUES ($1, $2, $3, $4)
			RETURNING created_at`, bookingID, eventID, actor, method).Scan(&result.CheckedInAt)
	})
	return result, err
}

// CheckInSummary lists confirmed bookings for the staff search and counts who has arrived.
type CheckInSummary struct {
	Confirmed int           `json:"confirmed"`
	CheckedIn int           `json:"checked_in"`
	Opens     time.Time     `json:"opens_at"`
	Closes    time.Time     `json:"closes_at"`
	Items     []CheckInItem `json:"items"`
}

// CheckInItem is one confirmed booking in the staff list.
type CheckInItem struct {
	BookingID    string     `json:"booking_id"`
	BusinessName string     `json:"business_name"`
	StandCode    string     `json:"stand_code,omitempty"`
	CheckedInAt  *time.Time `json:"checked_in_at"`
}

// ListCheckIns is for event staff. query narrows by business name.
func (s Store) ListCheckIns(ctx context.Context, actor, eventID, query string, limit int) (CheckInSummary, error) {
	var out CheckInSummary
	out.Items = []CheckInItem{}
	if !httpapi.ValidID(actor) || !httpapi.ValidID(eventID) {
		return out, ErrNotFound
	}
	err := pgx.BeginTxFunc(ctx, s.DB, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		staff, err := isStaff(ctx, tx, actor, eventID)
		if err != nil {
			return err
		}
		if !staff {
			return ErrNotFound
		}
		var starts time.Time
		if err := tx.QueryRow(ctx, `SELECT starts_at, ends_at FROM events WHERE id = $1`, eventID).Scan(&starts, &out.Closes); err != nil {
			return err
		}
		out.Opens = starts.Add(-CheckInLead)
		if err := tx.QueryRow(ctx, `SELECT count(*), count(i.booking_id) FROM bookings b
			LEFT JOIN check_ins i ON i.booking_id = b.id WHERE b.event_id = $1 AND b.status = 'confirmed'`, eventID).
			Scan(&out.Confirmed, &out.CheckedIn); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT b.id::text, z.name, COALESCE(s.code, ''), i.created_at FROM bookings b
			JOIN businesses z ON z.id = b.business_id LEFT JOIN check_ins i ON i.booking_id = b.id
			LEFT JOIN allocations a ON a.booking_id = b.id LEFT JOIN stands s ON s.id = a.stand_id
			WHERE b.event_id = $1 AND b.status = 'confirmed' AND ($2 = '' OR strpos(lower(z.name), lower($2)) > 0
				OR strpos(lower(COALESCE(s.code, '')), lower($2)) > 0)
			ORDER BY i.created_at IS NOT NULL, z.name, b.id LIMIT $3`, eventID, query, limit)
		if err != nil {
			return err
		}
		out.Items, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (CheckInItem, error) {
			var item CheckInItem
			err := row.Scan(&item.BookingID, &item.BusinessName, &item.StandCode, &item.CheckedInAt)
			return item, err
		})
		return err
	})
	return out, err
}
