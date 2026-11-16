package events

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"kitakarsa.com/tenantku/backend/internal/auth"
	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

var (
	errRevisionSuperseded = errors.New("event revision no longer matches the live event")
	errRevisionApproval   = errors.New("active bookings still need to approve the revision")
)

const revisionClosed = `EXISTS (
	SELECT 1 FROM event_revision_applications closed
	WHERE closed.event_id = r.event_id AND closed.supersedes_through >= r.revision
)`

const revisionStatusSQL = `CASE WHEN app.applied_at IS NOT NULL THEN 'applied'
	WHEN ` + revisionClosed + ` THEN 'superseded' ELSE 'proposed' END`

const unapprovedActiveBookings = `EXISTS (
	SELECT 1 FROM bookings b WHERE b.event_id = r.event_id AND (` + activeBooking + `)
	AND NOT EXISTS (
		SELECT 1 FROM event_revision_decisions d
		WHERE d.revision_id = r.id AND d.booking_id = b.id AND d.decision = 'approved'
	)
)`

func (s Store) applyEventRevision(c *gin.Context, account auth.Account) {
	id, revisionID := c.Param("id"), c.Param("revision_id")
	if !validResource(c, id) || !validResource(c, revisionID) {
		return
	}
	updated, err := s.applyRevision(c.Request.Context(), account.ID, id, revisionID)
	switch {
	case errors.Is(err, errRevisionSuperseded):
		httpapi.Error(c, http.StatusConflict, "EVENT_REVISION_SUPERSEDED", "Usulan sudah tidak sesuai dengan acara saat ini. Ajukan revisi baru.", nil)
	case errors.Is(err, errRevisionApproval):
		httpapi.Error(c, http.StatusConflict, "EVENT_REVISION_APPROVAL_REQUIRED", "Semua booking aktif harus menyetujui usulan. Booking baru memerlukan usulan baru.", nil)
	default:
		writeEvent(c, http.StatusOK, updated, err)
	}
}

func (s Store) applyRevision(ctx context.Context, actor, id, revisionID string) (event, error) {
	var updated event
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		// Proposals, tenant decisions and new holds take a conflicting lock on
		// this event. Approval and booking eligibility stay stable until commit.
		current, err := lockOwnedEvent(ctx, tx, actor, id)
		if err != nil {
			return err
		}
		proposal, err := scanRevision(tx.QueryRow(ctx, selectRevision+` WHERE r.id = $1 AND r.event_id = $2`, revisionID, id))
		if err != nil {
			return err
		}
		if proposal.Status == "applied" {
			updated = current
			return nil
		}
		if proposal.Status != "proposed" || !matchesMaterial(current, proposal.Before) {
			return errRevisionSuperseded
		}
		if current.Status != "published" && current.Status != "live" {
			return errRevisionUnavailable
		}
		if !proposal.CanApply {
			return errRevisionApproval
		}

		var now time.Time
		var conflictsWithHold bool
		next := proposal.After
		err = tx.QueryRow(ctx, `SELECT statement_timestamp(), EXISTS (
			SELECT 1 FROM bookings WHERE event_id = $1 AND status IN ('held', 'payment_pending') AND expires_at > $2
		)`, id, next.EndsAt).Scan(&now, &conflictsWithHold)
		if err != nil {
			return err
		}
		if !current.EndsAt.After(now) {
			return errRevisionUnavailable
		}
		if !next.EndsAt.After(now) || conflictsWithHold {
			return fieldErrors{"ends_at": "Waktu selesai harus masih di depan dan mencakup masa hold yang aktif."}
		}

		updated, err = scanEvent(tx.QueryRow(ctx, `UPDATE events AS e
			SET starts_at = $2, ends_at = $3, timezone = $4, city = $5, address = $6, terms = $7, updated_at = clock_timestamp()
			WHERE e.id = $1 RETURNING `+eventColumns,
			id, next.StartsAt, next.EndsAt, next.Timezone, next.City, next.Address, next.Terms))
		if err != nil {
			return err
		}
		if err := syncSlot(ctx, tx, updated); err != nil {
			return err
		}
		if err := checkPublication(ctx, tx, updated, LayoutInventory{}, now); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO event_revision_applications (revision_id, event_id, applied_by, supersedes_through)
			SELECT $1, $2, $3, max(revision) FROM event_revisions WHERE event_id = $2`, revisionID, id, actor)
		return err
	})
	return updated, err
}

func matchesMaterial(current event, before materialSnapshot) bool {
	before.StartsAt = before.StartsAt.UTC()
	before.EndsAt = before.EndsAt.UTC()
	return materialFields(current) == before
}
