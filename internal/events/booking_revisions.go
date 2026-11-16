package events

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"kitakarsa.com/tenantku/backend/internal/auth"
	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

type bookingRevision struct {
	ID        string           `json:"id"`
	Revision  int              `json:"revision"`
	Before    materialSnapshot `json:"before"`
	After     materialSnapshot `json:"after"`
	CreatedAt time.Time        `json:"created_at"`
	Decision  *string          `json:"decision"`
	DecidedAt *time.Time       `json:"decided_at"`
	CanDecide bool             `json:"can_decide"`
	Status    string           `json:"status"`
}

const activeRevisionBooking = `e.status IN ('published', 'live') AND (` + activeBooking + `)`

var (
	errDecisionClosed   = errors.New("booking revision decision is closed")
	errDecisionRecorded = errors.New("booking revision decision was already recorded")
)

func (s Store) listBookingRevisions(c *gin.Context, account auth.Account) {
	c.Header("Cache-Control", "private, no-store")
	id := c.Param("id")
	if !httpapi.ValidID(id) {
		httpapi.Error(c, http.StatusNotFound, "NOT_FOUND", "Booking tidak ditemukan.", nil)
		return
	}
	limit, err := strconv.Atoi(c.DefaultQuery("limit", "20"))
	if err != nil || limit < 1 || limit > 100 {
		httpapi.Error(c, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Gunakan limit antara 1 dan 100.", nil)
		return
	}
	before, beforeID, err := httpapi.DecodeCursor(c.Query("cursor"))
	if err != nil {
		httpapi.Error(c, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Gunakan cursor dari halaman sebelumnya.", nil)
		return
	}
	data, err := s.bookingRevisions(c.Request.Context(), account.ID, id, before, beforeID, limit+1)
	if errors.Is(err, pgx.ErrNoRows) {
		httpapi.Error(c, http.StatusNotFound, "NOT_FOUND", "Booking tidak ditemukan.", nil)
		return
	}
	if err != nil {
		httpapi.LogInternalError(c, "booking revision list failed", err)
		httpapi.Error(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Usulan revisi belum dapat dimuat.", nil)
		return
	}
	var next *string
	if len(data) > limit {
		data = data[:limit]
		last := data[len(data)-1]
		cursor := httpapi.EncodeCursor(last.CreatedAt, last.ID)
		next = &cursor
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{"next_cursor": next}})
}

func (s Store) bookingRevisions(ctx context.Context, actor, id string, before time.Time, beforeID string, limit int) ([]bookingRevision, error) {
	var revisions []bookingRevision
	err := pgx.BeginTxFunc(ctx, s.DB, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var eventID string
		err := tx.QueryRow(ctx, `SELECT b.event_id FROM bookings b WHERE b.id = $1 AND EXISTS (
			SELECT 1 FROM memberships m WHERE m.business_id = b.business_id AND m.user_id = $2 AND m.role = 'owner'
		)`, id, actor).Scan(&eventID)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT r.id, r.revision, r.snapshot_before, r.snapshot_after, r.created_at,
			d.decision, d.decided_at, d.decision IS NULL AND (`+activeRevisionBooking+`) AND NOT (`+revisionClosed+`),
			`+revisionStatusSQL+`
			FROM event_revisions r JOIN event_revision_bookings rb ON rb.revision_id = r.id AND rb.event_id = r.event_id
			JOIN bookings b ON b.id = rb.booking_id JOIN events e ON e.id = r.event_id
			LEFT JOIN event_revision_decisions d ON d.revision_id = r.id AND d.booking_id = b.id
			LEFT JOIN event_revision_applications app ON app.revision_id = r.id
			WHERE r.event_id = $1 AND rb.booking_id = $2 AND ($3::uuid IS NULL OR (r.created_at, r.id) < ($4, $3))
			ORDER BY r.created_at DESC, r.id DESC LIMIT $5`, eventID, id, nilIfEmpty(beforeID), before, limit)
		if err != nil {
			return err
		}
		revisions, err = pgx.AppendRows(make([]bookingRevision, 0), rows, func(row pgx.CollectableRow) (bookingRevision, error) {
			var revision bookingRevision
			err := row.Scan(&revision.ID, &revision.Revision, &revision.Before, &revision.After, &revision.CreatedAt,
				&revision.Decision, &revision.DecidedAt, &revision.CanDecide, &revision.Status)
			return revision, err
		})
		return err
	})
	return revisions, err
}

type revisionDecision struct {
	Decision  string    `json:"decision"`
	DecidedAt time.Time `json:"decided_at"`
}

func (s Store) decideBookingRevision(c *gin.Context, account auth.Account) {
	c.Header("Cache-Control", "private, no-store")
	id, revisionID := c.Param("id"), c.Param("revision_id")
	if !httpapi.ValidID(id) || !httpapi.ValidID(revisionID) {
		httpapi.Error(c, http.StatusNotFound, "NOT_FOUND", "Usulan revisi booking tidak ditemukan.", nil)
		return
	}
	type input struct {
		Decision string `json:"decision"`
	}
	body, ok := httpapi.ReadJSON[input](c, 512)
	if !ok {
		return
	}
	if body.Decision != "approved" && body.Decision != "declined" {
		httpapi.Error(c, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Pilih setujui atau tolak usulan.", nil)
		return
	}
	result, err := s.recordBookingRevisionDecision(c.Request.Context(), account.ID, id, revisionID, body.Decision)
	switch {
	case err == nil:
		httpapi.Success(c, http.StatusOK, result)
	case errors.Is(err, pgx.ErrNoRows):
		httpapi.Error(c, http.StatusNotFound, "NOT_FOUND", "Usulan revisi booking tidak ditemukan.", nil)
	case errors.Is(err, errDecisionClosed):
		httpapi.Error(c, http.StatusConflict, "REVISION_DECISION_CLOSED", "Usulan ini sudah tidak menerima keputusan.", nil)
	case errors.Is(err, errDecisionRecorded):
		httpapi.Error(c, http.StatusConflict, "REVISION_DECISION_RECORDED", "Keputusan untuk usulan ini sudah tercatat.", nil)
	default:
		httpapi.LogInternalError(c, "booking revision decision failed", err)
		httpapi.Error(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Keputusan belum dapat dicatat.", nil)
	}
}

func (s Store) recordBookingRevisionDecision(ctx context.Context, actor, id, revisionID, decision string) (revisionDecision, error) {
	var result revisionDecision
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var active bool
		err := tx.QueryRow(ctx, `SELECT `+activeRevisionBooking+`
			FROM event_revision_bookings rb
			JOIN bookings b ON b.id = rb.booking_id JOIN events e ON e.id = rb.event_id
			JOIN memberships m ON m.business_id = b.business_id AND m.user_id = $3 AND m.role = 'owner'
			WHERE rb.revision_id = $1 AND rb.booking_id = $2 FOR SHARE OF e, m`, revisionID, id, actor).Scan(&active)
		if err != nil {
			return err
		}
		var closed bool
		if active {
			// Read after obtaining the event lock to see any concurrent application.
			if err := tx.QueryRow(ctx, `SELECT `+revisionClosed+` FROM event_revisions r WHERE r.id = $1`, revisionID).Scan(&closed); err != nil {
				return err
			}
		}
		if active && !closed {
			err = tx.QueryRow(ctx, `INSERT INTO event_revision_decisions (revision_id, booking_id, decided_by, decision)
				VALUES ($1, $2, $3, $4) ON CONFLICT (revision_id, booking_id) DO NOTHING
				RETURNING decision, decided_at`, revisionID, id, actor, decision).Scan(&result.Decision, &result.DecidedAt)
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		err = tx.QueryRow(ctx, `SELECT decision, decided_at FROM event_revision_decisions WHERE revision_id = $1 AND booking_id = $2`,
			revisionID, id).Scan(&result.Decision, &result.DecidedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return errDecisionClosed
		}
		if err == nil && result.Decision != decision {
			return errDecisionRecorded
		}
		return err
	})
	return result, err
}
