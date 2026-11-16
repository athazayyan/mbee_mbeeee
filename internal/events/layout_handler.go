package events

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"kitakarsa.com/tenantku/backend/internal/auth"
	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

const maxLayoutInput = 512 << 10

// getPublicLayout serves the layout of a published or live event without a session.
func (s Store) getPublicLayout(c *gin.Context) {
	if s.DB == nil {
		httpapi.Error(c, http.StatusServiceUnavailable, "LAYOUT_UNAVAILABLE", "Denah belum dapat dimuat.", nil)
		return
	}
	id := c.Param("id")
	if !validResource(c, id) {
		return
	}
	layout, err := s.publicLayoutFor(c.Request.Context(), id)
	if err != nil {
		writeError(c, err)
		return
	}
	c.Header("ETag", layoutETag(layout.Revision))
	httpapi.Success(c, http.StatusOK, layout)
}

// getOrganizerLayout lets organization members read the layout at any event status, including drafts.
func (s Store) getOrganizerLayout(c *gin.Context, account auth.Account) {
	org, eventID := c.Param("id"), c.Param("event_id")
	if !validResource(c, org) || !validResource(c, eventID) {
		return
	}
	layout, err := s.organizerLayout(c.Request.Context(), account.ID, org, eventID)
	if err != nil {
		writeError(c, err)
		return
	}
	c.Header("ETag", layoutETag(layout.Revision))
	httpapi.Success(c, http.StatusOK, layout)
}

func (s Store) replaceLayout(c *gin.Context, account auth.Account) {
	id := c.Param("id")
	if !validResource(c, id) {
		return
	}
	input, ok := httpapi.ReadJSON[layoutInput](c, maxLayoutInput)
	if !ok {
		return
	}
	if fields := input.validate(); fields != nil {
		writeError(c, fields)
		return
	}
	layout, err := s.saveLayout(c.Request.Context(), account.ID, id, c.GetHeader("If-Match"), input)
	if err != nil {
		writeError(c, err)
		return
	}
	c.Header("ETag", layoutETag(layout.Revision))
	httpapi.Success(c, http.StatusOK, layout)
}

func (s Store) organizerLayout(ctx context.Context, actor, organization, eventID string) (layoutRevision, error) {
	var result layoutRevision
	err := pgx.BeginTxFunc(ctx, s.DB, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var found string
		err := tx.QueryRow(ctx, `SELECT e.id::text FROM events e JOIN memberships m
			ON m.organization_id = e.organization_id AND m.user_id = $1
			WHERE e.organization_id = $2 AND e.id = $3`, actor, organization, eventID).Scan(&found)
		if err != nil {
			return err
		}
		result, err = loadLayout(ctx, tx, eventID)
		if errors.Is(err, pgx.ErrNoRows) {
			// No layout yet: return an empty grid at revision 0 so the editor can create the first revision.
			result = layoutRevision{EventID: eventID, layoutInput: layoutInput{
				Width: 16, Height: 12, CellMeters: 1.5, Blocks: []layoutBlock{}, Stands: []layoutStand{}, Areas: []layoutArea{},
			}}
			return nil
		}
		return err
	})
	return result, err
}

// writeLayoutError maps layout-specific errors; it reports false for errors handled elsewhere.
func writeLayoutError(c *gin.Context, err error) bool {
	var reserved reservedStandError
	switch {
	case errors.Is(err, errLayoutConflict):
		httpapi.Error(c, http.StatusConflict, "LAYOUT_REVISION_CONFLICT",
			"Denah sudah diubah di tempat lain. Muat ulang denah lalu simpan lagi.", nil)
	case errors.Is(err, errLayoutPrecondition):
		httpapi.Error(c, http.StatusPreconditionRequired, "LAYOUT_REVISION_REQUIRED",
			"Sertakan header If-Match dari revisi denah terakhir.", nil)
	case errors.Is(err, errLayoutClosed):
		httpapi.Error(c, http.StatusConflict, "EVENT_CLOSED",
			"Denah acara yang sudah selesai atau dibatalkan tidak bisa diubah.", nil)
	case errors.As(err, &reserved):
		httpapi.Error(c, http.StatusConflict, "STAND_HAS_BOOKING",
			"Stand "+reserved.code+" memiliki booking aktif sehingga tidak bisa dihapus, dipindah, atau dinonaktifkan.",
			map[string]string{"stands": "Ajukan revisi acara untuk mengubah stand yang sudah dipesan."})
	default:
		return false
	}
	return true
}
