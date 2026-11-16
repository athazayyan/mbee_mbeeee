package events

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"kitakarsa.com/tenantku/backend/internal/auth"
	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

var (
	errRevisionRequired       = errors.New("material event changes require a revision")
	errRevisionUnavailable    = errors.New("event is not eligible for a revision")
	errIdempotencyKeyMismatch = errors.New("idempotency key was already used for a different request")
)

const activeBooking = `b.status = 'confirmed' OR (b.status IN ('held', 'payment_pending') AND b.expires_at > statement_timestamp())`

const affectedBookings = `FROM bookings b WHERE b.event_id = $1 AND (` + activeBooking + `)`

type materialInput struct {
	StartsAt *time.Time `json:"starts_at"`
	EndsAt   *time.Time `json:"ends_at"`
	Timezone *string    `json:"timezone"`
	City     *string    `json:"city"`
	Address  *string    `json:"address"`
	Terms    *string    `json:"terms"`
}

type revisionInput struct {
	Reason  string        `json:"reason"`
	Changes materialInput `json:"changes"`
}

type materialSnapshot struct {
	StartsAt time.Time `json:"starts_at"`
	EndsAt   time.Time `json:"ends_at"`
	Timezone string    `json:"timezone"`
	City     string    `json:"city"`
	Address  string    `json:"address"`
	Terms    string    `json:"terms"`
}

func materialFields(value event) materialSnapshot {
	return materialSnapshot{
		StartsAt: value.StartsAt.UTC(),
		EndsAt:   value.EndsAt.UTC(),
		Timezone: value.Timezone,
		City:     value.City,
		Address:  value.Address,
		Terms:    existing(value.Terms, ""),
	}
}

func revisionRequired(before, after event) bool {
	return (before.Status == "draft" || before.Status == "published" || before.Status == "live") &&
		materialFields(before) != materialFields(after)
}

func hasAffectedBookings(ctx context.Context, tx pgx.Tx, id string) (bool, error) {
	var active bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 `+affectedBookings+`)`, id).Scan(&active)
	return active, err
}

type eventRevision struct {
	ID               string           `json:"id"`
	EventID          string           `json:"event_id"`
	Revision         int              `json:"revision"`
	RequestedBy      string           `json:"requested_by"`
	Reason           string           `json:"reason"`
	Before           materialSnapshot `json:"before"`
	After            materialSnapshot `json:"after"`
	AffectedBookings int64            `json:"affected_bookings"`
	ApprovedBookings int64            `json:"approved_bookings"`
	DeclinedBookings int64            `json:"declined_bookings"`
	CreatedAt        time.Time        `json:"created_at"`
	Status           string           `json:"status"`
	CanApply         bool             `json:"can_apply"`
	AppliedAt        *time.Time       `json:"applied_at"`
}

const selectRevision = `SELECT r.id, r.event_id, r.revision, r.requested_by, r.reason,
	r.snapshot_before, r.snapshot_after, counts.affected, counts.approved, counts.declined, r.created_at,
	` + revisionStatusSQL + `, NOT (` + revisionClosed + `) AND NOT (` + unapprovedActiveBookings + `), app.applied_at
	FROM event_revisions r
	LEFT JOIN event_revision_applications app ON app.revision_id = r.id
	CROSS JOIN LATERAL (
		SELECT count(*) AS affected, count(*) FILTER (WHERE d.decision = 'approved') AS approved,
			count(*) FILTER (WHERE d.decision = 'declined') AS declined
		FROM event_revision_bookings b
		LEFT JOIN event_revision_decisions d ON d.revision_id = b.revision_id AND d.booking_id = b.booking_id
		WHERE b.revision_id = r.id
	) counts`

func (s Store) proposeRevision(c *gin.Context, account auth.Account) {
	id := c.Param("id")
	if !validResource(c, id) {
		return
	}
	key := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if !httpapi.ValidSingleLine(key, 128) || len(key) > 128 {
		httpapi.Error(c, http.StatusBadRequest, "INVALID_IDEMPOTENCY_KEY", "Sertakan Idempotency-Key dengan panjang 1–128 byte.", nil)
		return
	}
	input, ok := httpapi.ReadJSON[revisionInput](c, maxEventInput)
	if !ok {
		return
	}
	changes := eventInput{
		StartsAt: input.Changes.StartsAt, EndsAt: input.Changes.EndsAt, Timezone: input.Changes.Timezone,
		City: input.Changes.City, Address: input.Changes.Address, Terms: input.Changes.Terms,
	}
	fields := validateEventInput(changes, false, false)
	if changes == (eventInput{}) {
		delete(fields, "name")
		fields["changes"] = "Isi setidaknya satu perubahan material."
	}
	input.Reason = strings.TrimSpace(input.Reason)
	if input.Reason == "" || utf8.RuneCountInString(input.Reason) > 2000 || strings.ContainsRune(input.Reason, 0) {
		fields["reason"] = "Masukkan alasan dengan panjang 1–2.000 karakter."
	}
	if len(fields) != 0 {
		writeError(c, fields)
		return
	}
	payload, err := json.Marshal(input)
	if err != nil {
		writeError(c, err)
		return
	}
	digest := sha256.Sum256(payload)
	record, created, err := s.recordRevision(c.Request.Context(), account.ID, id, key, digest[:], input.Reason, changes)
	if err != nil {
		writeError(c, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	httpapi.Success(c, status, record)
}

func (s Store) recordRevision(ctx context.Context, actor, id, key string, digest []byte, reason string, changes eventInput) (eventRevision, bool, error) {
	var revision eventRevision
	var created bool
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		current, err := lockOwnedEvent(ctx, tx, actor, id)
		if err != nil {
			return err
		}
		var revisionID string
		var previousDigest []byte
		err = tx.QueryRow(ctx, `SELECT id, request_digest FROM event_revisions
			WHERE event_id = $1 AND requested_by = $2 AND idempotency_key = $3`, id, actor, key).Scan(&revisionID, &previousDigest)
		if err == nil {
			if !bytes.Equal(previousDigest, digest) {
				return errIdempotencyKeyMismatch
			}
			revision, err = scanRevision(tx.QueryRow(ctx, selectRevision+` WHERE r.id = $1`, revisionID))
			return err
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if current.Status != "published" && current.Status != "live" {
			return errRevisionUnavailable
		}
		before, after := materialFields(current), materialFields(changes.apply(current))
		if before == after {
			return fieldErrors{"changes": "Usulan sama dengan data acara saat ini."}
		}
		if !after.EndsAt.After(after.StartsAt) {
			return fieldErrors{"ends_at": "Waktu selesai harus sesudah waktu mulai."}
		}
		if strings.TrimSpace(after.Terms) == "" {
			return fieldErrors{"terms": "Acara terbit harus memiliki syarat yang berlaku."}
		}
		err = tx.QueryRow(ctx, `INSERT INTO event_revisions
			(event_id, revision, requested_by, reason, snapshot_before, snapshot_after, idempotency_key, request_digest)
			SELECT $1, COALESCE(max(revision), 0) + 1, $2, $3, $4, $5, $6, $7
			FROM event_revisions WHERE event_id = $1 RETURNING id`, id, actor, reason, before, after, key, digest).Scan(&revisionID)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `INSERT INTO event_revision_bookings (revision_id, event_id, booking_id, status_at_proposal)
			SELECT $2, b.event_id, b.id, b.status `+affectedBookings, id, revisionID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errRevisionUnavailable
		}
		revision, err = scanRevision(tx.QueryRow(ctx, selectRevision+` WHERE r.id = $1`, revisionID))
		created = err == nil
		return err
	})
	return revision, created, err
}

func (s Store) listRevisions(c *gin.Context, account auth.Account) {
	id := c.Param("id")
	if !validResource(c, id) {
		return
	}
	limit, err := strconv.Atoi(c.DefaultQuery("limit", "20"))
	if err != nil || limit < 1 || limit > 100 {
		writeError(c, fieldErrors{"limit": "Gunakan limit antara 1 dan 100."})
		return
	}
	before, beforeID, err := httpapi.DecodeCursor(c.Query("cursor"))
	if err != nil {
		writeError(c, fieldErrors{"cursor": "Gunakan cursor dari halaman sebelumnya."})
		return
	}
	data, err := s.revisions(c.Request.Context(), account.ID, id, before, beforeID, limit+1)
	if err != nil {
		writeError(c, err)
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

func (s Store) revisions(ctx context.Context, actor, id string, before time.Time, beforeID string, limit int) ([]eventRevision, error) {
	var revisions []eventRevision
	err := pgx.BeginTxFunc(ctx, s.DB, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var owned string
		err := tx.QueryRow(ctx, `SELECT e.id FROM events e JOIN memberships m ON m.organization_id = e.organization_id
			WHERE e.id = $1 AND m.user_id = $2 AND m.role = 'owner'`, id, actor).Scan(&owned)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, selectRevision+` WHERE r.event_id = $1
			AND ($2::uuid IS NULL OR (r.created_at, r.id) < ($3, $2)) ORDER BY r.created_at DESC, r.id DESC LIMIT $4`,
			owned, nilIfEmpty(beforeID), before, limit)
		if err != nil {
			return err
		}
		revisions, err = pgx.AppendRows(make([]eventRevision, 0), rows, func(row pgx.CollectableRow) (eventRevision, error) {
			return scanRevision(row)
		})
		return err
	})
	return revisions, err
}

func scanRevision(row pgx.Row) (eventRevision, error) {
	var r eventRevision
	err := row.Scan(&r.ID, &r.EventID, &r.Revision, &r.RequestedBy, &r.Reason,
		&r.Before, &r.After, &r.AffectedBookings, &r.ApprovedBookings, &r.DeclinedBookings, &r.CreatedAt,
		&r.Status, &r.CanApply, &r.AppliedAt)
	return r, err
}
