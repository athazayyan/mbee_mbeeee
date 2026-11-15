package events

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"kitakarsa.com/tenantku/backend/internal/auth"
	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

type priceInput struct {
	priceRules
	EffectiveAt *time.Time        `json:"effective_at"`
	StandPrices []json.RawMessage `json:"stand_prices"`
	BlockPrices []json.RawMessage `json:"block_prices"`
}

type pricePreviewInput struct {
	Rules          priceInput `json:"rules"`
	EventSlotID    string     `json:"event_slot_id"`
	ConfirmedCount *int32     `json:"confirmed_count"`
	Capacity       int32      `json:"capacity"`
}

type priceVersion struct {
	EventID     string     `json:"event_id"`
	Revision    string     `json:"price_version"`
	EffectiveAt time.Time  `json:"effective_at"`
	Rules       priceRules `json:"rules"`
}

type pricedSlot struct {
	ID       string    `json:"id"`
	Code     string    `json:"code"`
	StartsAt time.Time `json:"starts_at"`
	EndsAt   time.Time `json:"ends_at"`
}

type pricingState struct {
	Slots  []pricedSlot  `json:"slots"`
	Active *priceVersion `json:"active"`
	Latest *priceVersion `json:"latest"`
}

func (input *priceInput) fields() fieldErrors {
	fields := input.priceRules.validate()
	if len(input.StandPrices) != 0 {
		fields["stand_prices"] = "Harga per stand tersedia setelah denah terhubung."
	}
	if len(input.BlockPrices) != 0 {
		fields["block_prices"] = "Harga per blok tersedia setelah denah terhubung."
	}
	return fields
}

func (s Store) getPricing(c *gin.Context, account auth.Account) {
	id := c.Param("id")
	if !validResource(c, id) {
		return
	}
	ctx := c.Request.Context()
	var state pricingState
	err := pgx.BeginTxFunc(ctx, s.DB, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var eventID string
		err := tx.QueryRow(ctx, `SELECT e.id FROM events e JOIN memberships m ON e.organization_id = m.organization_id
			WHERE e.id = $1 AND m.user_id = $2`, id, account.ID).Scan(&eventID)
		if err != nil {
			return err
		}
		state.Slots, err = pricingSlots(ctx, tx, eventID)
		if err != nil {
			return err
		}
		state.Latest, err = loadPriceVersion(ctx, tx, `SELECT event_id, revision::text, effective_at, rules
			FROM event_pricing_versions WHERE event_id = $1 ORDER BY revision DESC LIMIT 1`, eventID)
		if err != nil {
			return err
		}
		state.Active, err = loadPriceVersion(ctx, tx, `SELECT event_id, revision::text, effective_at, rules
			FROM event_pricing_versions WHERE event_id = $1 AND effective_at <= transaction_timestamp()
			ORDER BY effective_at DESC, revision DESC LIMIT 1`, eventID)
		return err
	})
	if err != nil {
		writeError(c, err)
		return
	}
	httpapi.Success(c, http.StatusOK, state)
}

func (s Store) replacePricing(c *gin.Context, account auth.Account) {
	id := c.Param("id")
	if !validResource(c, id) {
		return
	}
	input, ok := httpapi.ReadJSON[priceInput](c, maxEventInput)
	if !ok {
		return
	}
	if fields := input.fields(); len(fields) != 0 {
		writeError(c, fields)
		return
	}
	ctx := c.Request.Context()
	var saved *priceVersion
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		value, err := lockOwnedEvent(ctx, tx, account.ID, id)
		if err != nil {
			return err
		}
		if value.Status != "draft" {
			return errDraftRequired
		}
		if err := validatePriceReferences(ctx, tx, id, input.priceRules, ""); err != nil {
			return err
		}
		var now time.Time
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
			return err
		}
		effective := now
		if input.EffectiveAt != nil {
			effective = *input.EffectiveAt
			if effective.Before(now) || !effective.Before(value.EndsAt) {
				return fieldErrors{"effective_at": "Pilih waktu mulai yang akan datang dan sebelum acara selesai."}
			}
		}
		saved, err = loadPriceVersion(ctx, tx, `INSERT INTO event_pricing_versions (event_id, revision, created_by, rules, effective_at)
			SELECT $1, COALESCE(MAX(revision), 0) + 1, $2, $3, $4 FROM event_pricing_versions WHERE event_id = $1
			RETURNING event_id, revision::text, effective_at, rules`, id, account.ID, input.priceRules, effective)
		return err
	})
	if err != nil {
		writeError(c, err)
		return
	}
	httpapi.Success(c, http.StatusOK, saved)
}

func (s Store) previewPrice(c *gin.Context, account auth.Account) {
	id := c.Param("id")
	if !validResource(c, id) {
		return
	}
	input, ok := httpapi.ReadJSON[pricePreviewInput](c, maxEventInput)
	if !ok {
		return
	}
	fields := input.Rules.fields()
	input.EventSlotID = strings.ToLower(input.EventSlotID)
	if !httpapi.ValidID(input.EventSlotID) {
		fields["event_slot_id"] = "Pilih slot acara yang valid."
	}
	if input.ConfirmedCount == nil {
		fields["confirmed_count"] = "Isi jumlah stand terkonfirmasi untuk simulasi."
	}
	if input.Capacity < 1 {
		fields["capacity"] = "Isi kapasitas simulasi di atas nol."
	}
	if len(fields) != 0 {
		writeError(c, fields)
		return
	}
	ctx := c.Request.Context()
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if _, err := lockOwnedEvent(ctx, tx, account.ID, id); err != nil {
			return err
		}
		return validatePriceReferences(ctx, tx, id, input.Rules.priceRules, input.EventSlotID)
	})
	if err != nil {
		writeError(c, err)
		return
	}
	quote, err := input.Rules.priceRules.quote(input.EventSlotID, *input.ConfirmedCount, input.Capacity)
	if err != nil {
		writeError(c, err)
		return
	}
	httpapi.Success(c, http.StatusOK, quote)
}

func pricingSlots(ctx context.Context, tx pgx.Tx, id string) ([]pricedSlot, error) {
	rows, err := tx.Query(ctx, `SELECT id, code, starts_at, ends_at FROM event_slots WHERE event_id = $1
		ORDER BY starts_at, id`, id)
	if err != nil {
		return nil, err
	}
	return pgx.AppendRows(make([]pricedSlot, 0), rows, pgx.RowToStructByPos[pricedSlot])
}

func loadPriceVersion(ctx context.Context, tx pgx.Tx, query string, args ...any) (*priceVersion, error) {
	var version priceVersion
	err := tx.QueryRow(ctx, query, args...).Scan(&version.EventID, &version.Revision, &version.EffectiveAt, &version.Rules)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &version, err
}

func validatePriceReferences(ctx context.Context, tx pgx.Tx, eventID string, rules priceRules, previewSlot string) error {
	slots, err := pricingSlots(ctx, tx, eventID)
	if err != nil {
		return err
	}
	known := make(map[string]bool, len(slots))
	for _, slot := range slots {
		known[slot.ID] = true
	}
	if previewSlot != "" && !known[previewSlot] {
		return fieldErrors{"event_slot_id": "Slot tidak tersedia pada acara ini."}
	}
	for _, slot := range rules.SlotPrices {
		if !known[slot.EventSlotID] {
			return fieldErrors{"slot_prices": "Gunakan slot yang terdaftar pada acara ini."}
		}
	}
	if len(rules.CategoryQuotas) == 0 {
		return nil
	}
	categories := make([]string, 0, len(rules.CategoryQuotas))
	for _, quota := range rules.CategoryQuotas {
		categories = append(categories, quota.CategoryID)
	}
	var count int
	err = tx.QueryRow(ctx, `SELECT count(*) FROM business_categories
		WHERE id = ANY($1::text[]::uuid[])`, categories).Scan(&count)
	if err != nil {
		return err
	}
	if count != len(categories) {
		return fieldErrors{"category_quotas": "Pilih kategori usaha yang tersedia."}
	}
	return nil
}
