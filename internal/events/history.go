package events

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"kitakarsa.com/tenantku/backend/internal/auth"
	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

type historyInput struct {
	Name     *string    `json:"name"`
	StartsAt *time.Time `json:"starts_at"`
	EndsAt   *time.Time `json:"ends_at"`
	Timezone *string    `json:"timezone"`
	City     *string    `json:"city"`
	Address  *string    `json:"address"`
}

type historicalDetails struct {
	Name     string    `json:"name"`
	StartsAt time.Time `json:"starts_at"`
	EndsAt   time.Time `json:"ends_at"`
	Timezone string    `json:"timezone"`
	City     string    `json:"city"`
	Address  string    `json:"address"`
}

type historicalEvent struct {
	ID             string    `json:"id"`
	OrganizationID string    `json:"organization_id"`
	Source         string    `json:"source"`
	ReportedAt     time.Time `json:"reported_at"`
	historicalDetails
}

const historyColumns = `id, organization_id, source, reported_at, name, starts_at, ends_at, timezone, city, address`

func (s Store) recordHistoricalEvent(c *gin.Context, account auth.Account) {
	id := c.Param("id")
	if !validResource(c, id) {
		return
	}
	key := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if !httpapi.ValidSingleLine(key, 128) || len(key) > 128 {
		httpapi.Error(c, http.StatusBadRequest, "INVALID_IDEMPOTENCY_KEY", "Sertakan Idempotency-Key dengan panjang 1–128 byte.", nil)
		return
	}
	input, ok := httpapi.ReadJSON[historyInput](c, maxEventInput)
	if !ok {
		return
	}
	fields := validateEventInput(eventInput{
		Name: input.Name, StartsAt: input.StartsAt, EndsAt: input.EndsAt, Timezone: input.Timezone,
		City: input.City, Address: input.Address,
	}, true, false)
	if len(fields) != 0 {
		writeError(c, fields)
		return
	}
	details := historicalDetails{
		Name: *input.Name, StartsAt: input.StartsAt.UTC().Truncate(time.Microsecond),
		EndsAt: input.EndsAt.UTC().Truncate(time.Microsecond), Timezone: *input.Timezone,
		City: *input.City, Address: *input.Address,
	}
	if !details.EndsAt.After(details.StartsAt) {
		writeError(c, fieldErrors{"ends_at": "Waktu selesai harus sesudah waktu mulai."})
		return
	}
	record, created, err := s.saveHistory(c.Request.Context(), account.ID, id, key, details)
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

func (s Store) saveHistory(ctx context.Context, actor, id, key string, details historicalDetails) (historicalEvent, bool, error) {
	var record historicalEvent
	var created bool
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var organization string
		if err := tx.QueryRow(ctx, `SELECT organization_id FROM memberships
			WHERE organization_id = $1 AND user_id = $2 AND role = 'owner' FOR SHARE`,
			id, actor).Scan(&organization); err != nil {
			return err
		}
		var err error
		record, err = scanHistoricalEvent(tx.QueryRow(ctx, `INSERT INTO organization_event_history
			(organization_id, reported_by, name, starts_at, ends_at, timezone, city, address, idempotency_key)
			SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9 WHERE $5 <= statement_timestamp()
			ON CONFLICT (organization_id, reported_by, idempotency_key) DO NOTHING RETURNING `+historyColumns,
			organization, actor, details.Name, details.StartsAt, details.EndsAt, details.Timezone, details.City, details.Address, key))
		if err == nil {
			created = true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		record, err = scanHistoricalEvent(tx.QueryRow(ctx, `SELECT `+historyColumns+` FROM organization_event_history
			WHERE organization_id = $1 AND reported_by = $2 AND idempotency_key = $3`, organization, actor, key))
		if errors.Is(err, pgx.ErrNoRows) {
			return fieldErrors{"ends_at": "Riwayat hanya dapat dicatat untuk acara yang sudah berakhir."}
		}
		if err != nil {
			return err
		}
		if !record.StartsAt.Equal(details.StartsAt) || !record.EndsAt.Equal(details.EndsAt) ||
			record.Name != details.Name || record.Timezone != details.Timezone ||
			record.City != details.City || record.Address != details.Address {
			return errIdempotencyKeyMismatch
		}
		return nil
	})
	return record, created, err
}

func (s Store) listHistoricalEvents(c *gin.Context, account auth.Account) {
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
	data, err := s.eventHistory(c.Request.Context(), account.ID, id, before, beforeID, limit+1)
	if err != nil {
		writeError(c, err)
		return
	}
	var next *string
	if len(data) > limit {
		data = data[:limit]
		last := data[len(data)-1]
		cursor := httpapi.EncodeCursor(last.StartsAt, last.ID)
		next = &cursor
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{"next_cursor": next}})
}

func (s Store) eventHistory(ctx context.Context, actor, id string, before time.Time, beforeID string, limit int) ([]historicalEvent, error) {
	var records []historicalEvent
	err := pgx.BeginTxFunc(ctx, s.DB, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var organization string
		if err := tx.QueryRow(ctx, `SELECT organization_id FROM memberships WHERE user_id = $1 AND organization_id = $2`,
			actor, id).Scan(&organization); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+historyColumns+` FROM organization_event_history
			WHERE organization_id = $1 AND ($2::uuid IS NULL OR (starts_at, id) < ($3, $2))
			ORDER BY starts_at DESC, id DESC LIMIT $4`, organization, nilIfEmpty(beforeID), before, limit)
		if err != nil {
			return err
		}
		records, err = pgx.AppendRows(make([]historicalEvent, 0), rows, func(row pgx.CollectableRow) (historicalEvent, error) {
			return scanHistoricalEvent(row)
		})
		return err
	})
	return records, err
}

func scanHistoricalEvent(row pgx.Row) (historicalEvent, error) {
	var record historicalEvent
	err := row.Scan(&record.ID, &record.OrganizationID, &record.Source, &record.ReportedAt,
		&record.Name, &record.StartsAt, &record.EndsAt, &record.Timezone, &record.City, &record.Address)
	return record, err
}
