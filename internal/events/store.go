package events

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const eventColumns = `e.id, e.organization_id, e.name, e.slug, e.status, e.starts_at,
	e.ends_at, e.timezone, e.city, e.address, e.description, e.facilities, e.terms`

var errDraftRequired = errors.New("event must be a draft")

type Store struct {
	DB               *pgxpool.Pool
	PricingInventory HoldInventory
}

type event struct {
	ID             string    `json:"id"`
	OrganizationID string    `json:"organization_id"`
	Name           string    `json:"name"`
	Slug           string    `json:"slug"`
	Status         string    `json:"status"`
	StartsAt       time.Time `json:"starts_at"`
	EndsAt         time.Time `json:"ends_at"`
	Timezone       string    `json:"timezone"`
	City           string    `json:"city"`
	Address        string    `json:"address"`
	Description    *string   `json:"description,omitempty"`
	Facilities     []string  `json:"facilities"`
	Terms          *string   `json:"terms,omitempty"`
}

func (s Store) Ready(ctx context.Context) error {
	_, err := s.DB.Exec(ctx, `SELECT 1 FROM events, event_slots, event_status_history, event_pricing_versions,
		event_revisions, event_revision_bookings, event_revision_decisions, event_revision_applications, organization_event_history,
		event_layouts, layout_blocks, layout_areas, stands, event_layout_revisions LIMIT 0`)
	return err
}

func (s Store) list(ctx context.Context, actor, organization string, before time.Time, beforeID string, limit int, query, status string) ([]event, error) {
	var result []event
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var org string
		err := tx.QueryRow(ctx, `SELECT organization_id FROM memberships
			WHERE user_id = $1 AND organization_id = $2 FOR SHARE`, actor, organization).Scan(&org)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+eventColumns+` FROM events e
			WHERE e.organization_id = $1 AND ($2::uuid IS NULL OR (e.starts_at, e.id) < ($3, $2))
			AND ($5 = '' OR strpos(lower(e.name), lower($5)) > 0 OR strpos(lower(e.city), lower($5)) > 0)
			AND ($6 = '' OR e.status = $6)
			ORDER BY e.starts_at DESC, e.id DESC LIMIT $4`, org, nilIfEmpty(beforeID), before, limit, query, status)
		if err != nil {
			return err
		}
		result, err = pgx.AppendRows(make([]event, 0), rows, pgx.RowToStructByPos[event])
		return err
	})
	return result, err
}

func nilIfEmpty(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func (s Store) get(ctx context.Context, actor, organization, id string) (event, error) {
	return scanEvent(s.DB.QueryRow(ctx, `SELECT `+eventColumns+` FROM events e
		JOIN memberships m ON m.organization_id = e.organization_id AND m.user_id = $1
		WHERE e.organization_id = $2 AND e.id = $3`, actor, organization, id))
}

func lockOwnedEvent(ctx context.Context, tx pgx.Tx, actor, id string) (event, error) {
	return scanEvent(tx.QueryRow(ctx, `SELECT `+eventColumns+` FROM events e
		JOIN memberships m ON m.organization_id = e.organization_id AND m.user_id = $1 AND m.role = 'owner'
		WHERE e.id = $2 FOR UPDATE OF e FOR SHARE OF m`, actor, id))
}

func (s Store) create(ctx context.Context, actor, organization string, input eventInput) (event, error) {
	var result event
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var org string
		err := tx.QueryRow(ctx, `SELECT organization_id FROM memberships
			WHERE user_id = $1 AND organization_id = $2 AND role = 'owner' FOR SHARE`, actor, organization).Scan(&org)
		if err != nil {
			return err
		}
		result, err = insertEvent(ctx, tx, actor, input.apply(event{OrganizationID: org, Facilities: []string{}}))
		return err
	})
	return result, err
}

func insertEvent(ctx context.Context, tx pgx.Tx, actor string, value event) (event, error) {
	created, err := scanEvent(tx.QueryRow(ctx, `INSERT INTO events AS e
		(organization_id, created_by, name, slug, starts_at, ends_at, timezone, city, address, description, facilities, terms)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12) RETURNING `+eventColumns,
		value.OrganizationID, actor, value.Name, value.Slug, value.StartsAt, value.EndsAt,
		value.Timezone, value.City, value.Address, value.Description, value.Facilities, value.Terms))
	if err != nil {
		return event{}, err
	}
	if err := syncSlot(ctx, tx, created); err != nil {
		return event{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO event_status_history (event_id, actor_id, to_status)
		VALUES ($1, $2, 'draft')`, created.ID, actor)
	return created, err
}

func (s Store) update(ctx context.Context, actor, id string, input eventInput) (event, error) {
	var result event
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		previous, err := lockOwnedEvent(ctx, tx, actor, id)
		if err != nil {
			return err
		}
		value := input.apply(previous)
		if revisionRequired(previous, value) {
			active, err := hasAffectedBookings(ctx, tx, id)
			if err != nil {
				return err
			}
			if active {
				return errRevisionRequired
			}
		}
		if previous.Status != "draft" {
			return errDraftRequired
		}
		if !value.EndsAt.After(value.StartsAt) {
			return fieldErrors{"ends_at": "Waktu selesai harus sesudah waktu mulai."}
		}
		result, err = scanEvent(tx.QueryRow(ctx, `UPDATE events AS e SET
			name = $2, slug = $3, starts_at = $4, ends_at = $5, timezone = $6, city = $7,
			address = $8, description = $9, facilities = $10, terms = $11, updated_at = clock_timestamp()
			WHERE e.id = $1 RETURNING `+eventColumns,
			id, value.Name, value.Slug, value.StartsAt, value.EndsAt, value.Timezone, value.City,
			value.Address, value.Description, value.Facilities, value.Terms))
		if err != nil {
			return err
		}
		return syncSlot(ctx, tx, result)
	})
	return result, err
}

func syncSlot(ctx context.Context, tx pgx.Tx, value event) error {
	_, err := tx.Exec(ctx, `INSERT INTO event_slots (event_id, code, starts_at, ends_at)
		VALUES ($1, 'whole-event', $2, $3) ON CONFLICT (event_id, code)
		DO UPDATE SET starts_at = EXCLUDED.starts_at, ends_at = EXCLUDED.ends_at`,
		value.ID, value.StartsAt, value.EndsAt)
	return err
}

func scanEvent(row pgx.Row) (event, error) {
	var value event
	err := row.Scan(&value.ID, &value.OrganizationID, &value.Name, &value.Slug, &value.Status,
		&value.StartsAt, &value.EndsAt, &value.Timezone, &value.City, &value.Address,
		&value.Description, &value.Facilities, &value.Terms)
	return value, err
}
