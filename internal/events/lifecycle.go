package events

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"kitakarsa.com/tenantku/backend/internal/auth"
	"kitakarsa.com/tenantku/backend/internal/funds"
	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

var errCancelReasonChanged = errors.New("event was already cancelled with a different reason")
var errCancellationNeedsBookingWorkflow = errors.New("event cancellation requires booking workflow")
var errEventNotCancellable = errors.New("event status does not allow cancellation")

type duplicateInput struct {
	Name string `json:"name"`
	Slug string `json:"slug"`
}

func (s Store) duplicateEvent(c *gin.Context, account auth.Account) {
	id := c.Param("id")
	if !validResource(c, id) {
		return
	}
	input, ok := httpapi.ReadJSON[duplicateInput](c, 4096)
	if !ok {
		return
	}
	input.Name, input.Slug = strings.TrimSpace(input.Name), strings.TrimSpace(input.Slug)
	fields := make(fieldErrors)
	if !httpapi.ValidSingleLine(input.Name, 160) {
		fields["name"] = "Masukkan nama acara dengan panjang 1–160 karakter."
	}
	if len(input.Slug) > 120 || !slugPattern.MatchString(input.Slug) {
		fields["slug"] = "Gunakan huruf kecil, angka, dan tanda hubung dengan panjang maksimal 120 karakter."
	}
	if len(fields) != 0 {
		writeError(c, fields)
		return
	}

	ctx := c.Request.Context()
	var created event
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		source, err := lockOwnedEvent(ctx, tx, account.ID, id)
		if err != nil {
			return err
		}
		source.Name, source.Slug = input.Name, input.Slug
		created, err = insertEvent(ctx, tx, account.ID, source)
		return err
	})
	writeEvent(c, http.StatusCreated, created, err)
}

type cancelInput struct {
	Reason string `json:"reason"`
}

func (s Store) cancelEvent(c *gin.Context, account auth.Account) {
	id := c.Param("id")
	if !validResource(c, id) {
		return
	}
	key := c.GetHeader("Idempotency-Key")
	if !httpapi.ValidSingleLine(strings.TrimSpace(key), 128) || len(key) > 128 {
		httpapi.Error(c, http.StatusBadRequest, "INVALID_IDEMPOTENCY_KEY", "Sertakan Idempotency-Key dengan panjang 1–128 byte.", nil)
		return
	}
	input, ok := httpapi.ReadJSON[cancelInput](c, 16<<10)
	if !ok {
		return
	}
	reason := strings.TrimSpace(input.Reason)
	if reason == "" || utf8.RuneCountInString(reason) > 2000 || strings.ContainsRune(reason, 0) {
		writeError(c, fieldErrors{"reason": "Masukkan alasan pembatalan dengan panjang 1–2.000 karakter."})
		return
	}
	data, err := s.cancel(c.Request.Context(), account.ID, id, reason)
	switch {
	case errors.Is(err, errCancelReasonChanged):
		httpapi.Error(c, http.StatusConflict, "EVENT_ALREADY_CANCELLED", "Acara sudah dibatalkan dengan alasan berbeda.", nil)
		return
	case errors.Is(err, errCancellationNeedsBookingWorkflow):
		httpapi.Error(c, http.StatusConflict, "EVENT_CANCELLATION_NEEDS_BOOKING_WORKFLOW",
			"Ada booking yang memerlukan penanganan pembayaran atau alokasi sebelum acara bisa dibatalkan.", nil)
		return
	case errors.Is(err, errEventNotCancellable):
		httpapi.Error(c, http.StatusConflict, "EVENT_NOT_CANCELLABLE", "Status acara tidak mengizinkan pembatalan.", nil)
		return
	}
	writeEvent(c, http.StatusOK, data, err)
}

func (s Store) cancel(ctx context.Context, actor, id, reason string) (event, error) {
	var cancelled event
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		previous, err := lockOwnedEvent(ctx, tx, actor, id)
		if err != nil {
			return err
		}
		switch previous.Status {
		case "cancelled":
			var recorded string
			err := tx.QueryRow(ctx, `SELECT reason FROM event_status_history
				WHERE event_id = $1 AND from_status IN ('draft', 'published', 'live') AND to_status = 'cancelled' AND reason IS NOT NULL
				ORDER BY id DESC LIMIT 1`, id).Scan(&recorded)
			if errors.Is(err, pgx.ErrNoRows) {
				return errEventNotCancellable
			}
			if err != nil {
				return err
			}
			if recorded != reason {
				return errCancelReasonChanged
			}
			cancelled = previous
			return nil
		case "draft", "published", "live":
		default:
			return errEventNotCancellable
		}
		cancelled, err = scanEvent(tx.QueryRow(ctx, `UPDATE events AS e
			SET status = 'cancelled', updated_at = clock_timestamp()
			WHERE e.id = $1 RETURNING `+eventColumns, id))
		if err != nil {
			return err
		}
		// Hold creation takes the same event lock. Any failure here rolls back
		// the event status as well as booking and allocation changes.
		var hasBookings bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM bookings WHERE event_id = $1)`, id).Scan(&hasBookings); err != nil {
			return err
		}
		if hasBookings {
			// Holds and offers are released, unpaid payments are closed, and paid bookings are refunded (E09-05).
			if _, err := funds.SettleCancellation(ctx, tx, id, actor); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `INSERT INTO event_status_history
			(event_id, actor_id, from_status, to_status, reason)
			VALUES ($1, $2, $3, 'cancelled', $4)`, id, actor, previous.Status, reason)
		return err
	})
	return cancelled, err
}
