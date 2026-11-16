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
}

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
		rows, err := tx.Query(ctx, `SELECT r.id, r.revision, r.snapshot_before, r.snapshot_after, r.created_at
			FROM event_revisions r JOIN event_revision_bookings b ON b.revision_id = r.id AND b.event_id = r.event_id
			WHERE r.event_id = $1 AND b.booking_id = $2 AND ($3::uuid IS NULL OR (r.created_at, r.id) < ($4, $3))
			ORDER BY r.created_at DESC, r.id DESC LIMIT $5`, eventID, id, nilIfEmpty(beforeID), before, limit)
		if err != nil {
			return err
		}
		revisions, err = pgx.AppendRows(make([]bookingRevision, 0), rows, func(row pgx.CollectableRow) (bookingRevision, error) {
			var revision bookingRevision
			err := row.Scan(&revision.ID, &revision.Revision, &revision.Before, &revision.After, &revision.CreatedAt)
			return revision, err
		})
		return err
	})
	return revisions, err
}
