package events

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Statuses visitors can see. Drafts never leave the organization.
var (
	catalogDefaultStatuses = []string{"published", "live"}
	catalogPublicStatuses  = []string{"published", "live", "completed", "cancelled"}
)

type publicEvent struct {
	ID               string    `json:"id"`
	OrganizationID   string    `json:"organization_id"`
	OrganizationName string    `json:"organization_name"`
	Status           string    `json:"status"`
	Name             string    `json:"name"`
	Slug             string    `json:"slug"`
	StartsAt         time.Time `json:"starts_at"`
	EndsAt           time.Time `json:"ends_at"`
	Timezone         string    `json:"timezone"`
	City             string    `json:"city"`
	Address          string    `json:"address"`
	Description      *string   `json:"description,omitempty"`
	Facilities       []string  `json:"facilities"`
	Terms            *string   `json:"terms,omitempty"`
	MinRent          *int64    `json:"min_rent,omitempty"`
	MaxRent          *int64    `json:"max_rent,omitempty"`
	Capacity         *int32    `json:"capacity,omitempty"`
	// Slots hanya diisi pada detail acara; tenant memilih slot sebelum meminta hold.
	Slots []publicSlot `json:"slots,omitempty"`
}

type publicSlot struct {
	ID       string    `json:"id"`
	Code     string    `json:"code"`
	StartsAt time.Time `json:"starts_at"`
	EndsAt   time.Time `json:"ends_at"`
}

type catalogFilter struct {
	Slug           string
	City           string
	CategoryID     string
	OrganizationID string
	Query          string
	Status         string
	StartsFrom     *time.Time
	EndsBefore     *time.Time
	AfterAt        time.Time
	AfterID        string
	Limit          int
}

func (filter catalogFilter) statuses() []string {
	switch {
	case filter.Status != "":
		return []string{filter.Status}
	case filter.Slug != "":
		// Looking up one event by slug also shows completed and cancelled events.
		return catalogPublicStatuses
	default:
		return catalogDefaultStatuses
	}
}

const publicEventColumns = `e.id::text, e.organization_id::text, o.name, e.status, e.name, e.slug, e.starts_at,
	e.ends_at, e.timezone, e.city, e.address, e.description, e.facilities, e.terms`

func scanPublicEvent(row pgx.Row) (publicEvent, error) {
	var value publicEvent
	err := row.Scan(&value.ID, &value.OrganizationID, &value.OrganizationName, &value.Status, &value.Name, &value.Slug,
		&value.StartsAt, &value.EndsAt, &value.Timezone, &value.City, &value.Address, &value.Description,
		&value.Facilities, &value.Terms)
	return value, err
}

// listPublic returns up to filter.Limit events after the cursor, ordered by start time.
func (s Store) listPublic(ctx context.Context, filter catalogFilter) ([]publicEvent, error) {
	var result []publicEvent
	err := pgx.BeginTxFunc(ctx, s.DB, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+publicEventColumns+` FROM events e
			JOIN organizations o ON o.id = e.organization_id
			WHERE e.status = ANY($1::text[])
			AND ($2 = '' OR e.slug = $2)
			AND ($3 = '' OR lower(e.city) = lower($3))
			AND ($4::uuid IS NULL OR e.organization_id = $4::uuid)
			AND ($5::timestamptz IS NULL OR e.starts_at >= $5)
			AND ($6::timestamptz IS NULL OR e.ends_at <= $6)
			AND ($7 = '' OR strpos(lower(e.name), lower($7)) > 0 OR strpos(lower(e.city), lower($7)) > 0
				OR strpos(lower(COALESCE(e.description, '')), lower($7)) > 0)
			AND ($8::uuid IS NULL OR EXISTS (SELECT 1 FROM stands s WHERE s.event_id = e.id
				AND s.removed_at IS NULL AND s.active
				AND (cardinality(s.category_ids) = 0 OR $8::uuid = ANY(s.category_ids))))
			AND ($9::uuid IS NULL OR (e.starts_at, e.id) > ($10, $9::uuid))
			ORDER BY e.starts_at, e.id LIMIT $11`,
			filter.statuses(), filter.Slug, filter.City, nilIfEmpty(filter.OrganizationID), filter.StartsFrom,
			filter.EndsBefore, filter.Query, nilIfEmpty(filter.CategoryID), nilIfEmpty(filter.AfterID), filter.AfterAt, filter.Limit)
		if err != nil {
			return err
		}
		result, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (publicEvent, error) { return scanPublicEvent(row) })
		if err != nil {
			return err
		}
		for i := range result {
			if err := addLayoutSummary(ctx, tx, &result[i]); err != nil {
				return err
			}
		}
		return nil
	})
	return result, err
}

// getPublic returns any event except drafts, which stay hidden as not found.
func (s Store) getPublic(ctx context.Context, id string) (publicEvent, error) {
	var result publicEvent
	err := pgx.BeginTxFunc(ctx, s.DB, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var err error
		result, err = scanPublicEvent(tx.QueryRow(ctx, `SELECT `+publicEventColumns+` FROM events e
			JOIN organizations o ON o.id = e.organization_id
			WHERE e.id = $1 AND e.status = ANY($2::text[])`, id, catalogPublicStatuses))
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT id::text, code, starts_at, ends_at FROM event_slots
			WHERE event_id = $1 ORDER BY starts_at, id`, id)
		if err != nil {
			return err
		}
		result.Slots, err = pgx.CollectRows(rows, pgx.RowToStructByPos[publicSlot])
		if err != nil {
			return err
		}
		return addLayoutSummary(ctx, tx, &result)
	})
	return result, err
}

// addLayoutSummary fills capacity and the rent range from the layout and the active pricing rules.
func addLayoutSummary(ctx context.Context, tx pgx.Tx, value *publicEvent) error {
	layout, err := loadLayout(ctx, tx, value.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var capacity int32
	for _, stand := range layout.Stands {
		if stand.Active {
			capacity++
		}
	}
	value.Capacity = &capacity
	taken, err := takenStands(ctx, tx, value.ID)
	if err != nil {
		return err
	}
	prices, err := standPrices(ctx, tx, value.ID, layout, taken)
	if err != nil {
		return err
	}
	for _, price := range prices {
		if price == nil {
			continue
		}
		if value.MinRent == nil || *price < *value.MinRent {
			value.MinRent = price
		}
		if value.MaxRent == nil || *price > *value.MaxRent {
			value.MaxRent = price
		}
	}
	return nil
}

// takenStands maps stand IDs with an active hold, offer, or reservation to the strongest allocation status.
func takenStands(ctx context.Context, tx pgx.Tx, eventID string) (map[string]string, error) {
	taken := make(map[string]string)
	rows, err := tx.Query(ctx, `SELECT a.stand_id::text, a.status FROM allocations a
		WHERE a.event_id = $1 AND a.status IN ('hold', 'offer', 'reserved')`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var stand, allocation string
		if err := rows.Scan(&stand, &allocation); err != nil {
			return nil, err
		}
		if taken[stand] != "reserved" {
			taken[stand] = allocation
		}
	}
	return taken, rows.Err()
}
