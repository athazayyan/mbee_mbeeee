package events

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"kitakarsa.com/tenantku/backend/internal/auth"
	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

type priceInput struct {
	priceRules
	EffectiveAt         *time.Time `json:"effective_at"`
	QuotaOverrideReason *string    `json:"quota_override_reason,omitempty"`
	QuotaBaseVersion    string     `json:"quota_base_version,omitempty"`
}

type pricePreviewInput struct {
	Rules          priceInput `json:"rules"`
	EventSlotID    string     `json:"event_slot_id"`
	StandID        *string    `json:"stand_id"`
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
	Slots             []pricedSlot      `json:"slots"`
	Active            *priceVersion     `json:"active"`
	Latest            *priceVersion     `json:"latest"`
	Inventory         *pricingInventory `json:"inventory,omitempty"`
	CanOverrideQuotas bool              `json:"can_override_quotas"`
}

const activePricingQuery = `SELECT event_id, revision::text, effective_at, rules
	FROM event_pricing_versions WHERE event_id = $1
	AND effective_at <= COALESCE($2::timestamptz, transaction_timestamp())
	ORDER BY effective_at DESC, revision DESC LIMIT 1`

func (s Store) getPricing(c *gin.Context, account auth.Account) {
	id := c.Param("id")
	if !validResource(c, id) {
		return
	}
	ctx := c.Request.Context()
	var state pricingState
	err := pgx.BeginTxFunc(ctx, s.DB, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var eventID string
		err := tx.QueryRow(ctx, `SELECT e.id, m.role = 'owner' AND EXISTS (SELECT 1 FROM platform_admins WHERE user_id = $2)
			FROM events e JOIN memberships m ON e.organization_id = m.organization_id
			WHERE e.id = $1 AND m.user_id = $2`, id, account.ID).Scan(&eventID, &state.CanOverrideQuotas)
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
		state.Active, err = loadPriceVersion(ctx, tx, activePricingQuery, eventID, nil)
		if err == nil && s.PricingInventory != nil {
			state.Inventory, err = readPricingInventory(ctx, tx, s.PricingInventory, eventID, state.Slots)
		}
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
	if fields := input.priceRules.validate(); len(fields) != 0 {
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
		published := value.Status == "published" || value.Status == "live"
		if value.Status != "draft" && !published {
			return errPricingClosed
		}
		if err := s.withPlan(ctx, tx, id, nil); err != nil {
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
		if published {
			if !effective.Before(value.EndsAt) {
				return errPricingClosed
			}
			if input.QuotaOverrideReason == nil {
				if err := retainPublishedQuotas(ctx, tx, id, effective, &input.priceRules); err != nil {
					return err
				}
			} else {
				if err := verifyQuotaOverride(ctx, tx, account.ID, id, effective, input); err != nil {
					return err
				}
			}
		} else if input.QuotaOverrideReason != nil {
			return fieldErrors{"quota_override_reason": "Override hanya berlaku untuk acara terbit atau berlangsung."}
		}
		if _, err := s.validatePriceReferences(ctx, tx, id, input.priceRules, "", ""); err != nil {
			return err
		}
		if published {
			slots, err := pricingSlots(ctx, tx, id)
			if err != nil {
				return err
			}
			if !coversOpenSlots(input.priceRules, slots, effective) {
				return fieldErrors{"pricing": "Tetapkan harga untuk setiap slot yang masih tersedia saat versi mulai berlaku."}
			}
		}
		saved, err = loadPriceVersion(ctx, tx, `INSERT INTO event_pricing_versions (event_id, revision, created_by, rules, effective_at)
			SELECT $1, COALESCE(MAX(revision), 0) + 1, $2, $3, $4 FROM event_pricing_versions WHERE event_id = $1
			RETURNING event_id, revision::text, effective_at, rules`, id, account.ID, input.priceRules, effective)
		if err != nil || input.QuotaOverrideReason == nil {
			return err
		}
		return writeQuotaOverrideAudit(ctx, tx, account.ID, id, c.Writer.Header().Get("X-Request-ID"), effective, input, saved.Revision)
	})
	if err != nil {
		if errors.Is(err, errQuotaOverrideForbidden) {
			httpapi.Error(c, http.StatusForbidden, "QUOTA_OVERRIDE_FORBIDDEN", "Perubahan kuota acara terbit memerlukan izin admin platform.", nil)
			return
		}
		writeError(c, err)
		return
	}
	httpapi.Success(c, http.StatusOK, saved)
}

var errPricingClosed = errors.New("event pricing can no longer be changed")

func retainPublishedQuotas(ctx context.Context, tx pgx.Tx, eventID string, at time.Time, rules *priceRules) error {
	previous, err := loadPriceVersion(ctx, tx, activePricingQuery, eventID, at)
	if err != nil {
		return err
	}
	var existing []categoryQuota
	if previous != nil {
		existing = previous.Rules.CategoryQuotas
	}
	if rules.CategoryQuotas != nil {
		caps := make(map[[2]string]*int32, len(existing))
		for _, quota := range existing {
			caps[[2]string{quota.CategoryID, quota.BlockID}] = quota.Cap
		}
		if len(rules.CategoryQuotas) != len(existing) || slices.ContainsFunc(rules.CategoryQuotas, func(quota categoryQuota) bool {
			cap := caps[[2]string{quota.CategoryID, quota.BlockID}]
			return cap == nil || *cap != *quota.Cap
		}) {
			return fieldErrors{"category_quotas": "Kuota kategori acara terbit mengikuti aturan yang sudah berlaku."}
		}
	}
	rules.CategoryQuotas = existing
	return nil
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
	fields := input.Rules.priceRules.validate()
	input.EventSlotID = strings.ToLower(input.EventSlotID)
	if !httpapi.ValidID(input.EventSlotID) {
		fields["event_slot_id"] = "Pilih slot acara yang valid."
	}
	standID := ""
	if input.StandID != nil {
		standID = strings.ToLower(*input.StandID)
		if !httpapi.ValidID(standID) {
			fields["stand_id"] = "Pilih stand yang valid."
		}
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
	var target priceTarget
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if _, err := lockOwnedEvent(ctx, tx, account.ID, id); err != nil {
			return err
		}
		var err error
		target, err = s.validatePriceReferences(ctx, tx, id, input.Rules.priceRules, input.EventSlotID, standID)
		return err
	})
	if err != nil {
		writeError(c, err)
		return
	}
	quote, err := input.Rules.priceRules.quote(target, *input.ConfirmedCount, input.Capacity)
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

func (s Store) validatePriceReferences(ctx context.Context, tx pgx.Tx, eventID string, rules priceRules, previewSlot, standID string) (priceTarget, error) {
	slots, err := pricingSlots(ctx, tx, eventID)
	if err != nil {
		return priceTarget{}, err
	}
	known := make(map[string]bool, len(slots))
	for _, slot := range slots {
		known[slot.ID] = true
	}
	if previewSlot != "" && !known[previewSlot] {
		return priceTarget{}, fieldErrors{"event_slot_id": "Slot tidak tersedia pada acara ini."}
	}
	for _, slot := range rules.SlotPrices {
		if !known[slot.EventSlotID] {
			return priceTarget{}, fieldErrors{"slot_prices": "Gunakan slot yang terdaftar pada acara ini."}
		}
	}
	if err := checkPricingCategories(ctx, tx, rules.CategoryQuotas); err != nil {
		return priceTarget{}, err
	}
	return s.validatePricingInventory(ctx, tx, eventID, slots, rules, previewSlot, standID)
}

func checkPricingCategories(ctx context.Context, tx pgx.Tx, quotas []categoryQuota) error {
	if len(quotas) == 0 {
		return nil
	}
	categories := make([]string, 0, len(quotas))
	seen := make(map[string]bool, len(quotas))
	for _, quota := range quotas {
		if !seen[quota.CategoryID] {
			categories = append(categories, quota.CategoryID)
			seen[quota.CategoryID] = true
		}
	}
	var count int
	err := tx.QueryRow(ctx, `SELECT count(*) FROM business_categories
		WHERE id = ANY($1::text[]::uuid[])`, categories).Scan(&count)
	if err != nil {
		return err
	}
	if count != len(categories) {
		return fieldErrors{"category_quotas": "Pilih kategori usaha yang tersedia."}
	}
	return nil
}

func (s Store) validatePricingInventory(ctx context.Context, tx pgx.Tx, eventID string, slots []pricedSlot, rules priceRules, previewSlot, standID string) (priceTarget, error) {
	target := priceTarget{slotID: previewSlot}
	hasBlockQuotas := slices.ContainsFunc(rules.CategoryQuotas, func(quota categoryQuota) bool { return quota.BlockID != "" })
	hasLayoutRules := len(rules.StandPrices) > 0 || len(rules.BlockPrices) > 0 || hasBlockQuotas
	if !hasLayoutRules && standID == "" {
		return target, nil
	}
	if s.PricingInventory == nil {
		if hasBlockQuotas {
			return target, fieldErrors{"category_quotas": "Kuota blok tersedia setelah denah terhubung."}
		}
		return target, fieldErrors{"layout": "Harga stand dan blok tersedia setelah denah terhubung."}
	}
	stands, blocks := make(map[string]bool), make(map[string]bool)
	for _, slot := range slots {
		if !hasLayoutRules && slot.ID != previewSlot {
			continue
		}
		inventory, err := s.PricingInventory.ActiveStands(ctx, tx, eventID, slot.ID)
		if err != nil {
			return target, err
		}
		for index := range inventory {
			stand := &inventory[index]
			stands[stand.ID] = true
			if stand.BlockID != nil {
				blocks[*stand.BlockID] = true
			}
			if slot.ID == previewSlot && stand.ID == standID {
				target.stand = stand
			}
		}
	}
	fields := make(fieldErrors)
	if standID != "" && target.stand == nil {
		fields["stand_id"] = "Stand tidak aktif pada slot acara ini."
	}
	for _, price := range rules.StandPrices {
		if !stands[price.StandID] {
			fields["stand_prices"] = "Gunakan stand aktif yang terdaftar pada acara ini."
			break
		}
	}
	for _, price := range rules.BlockPrices {
		if !blocks[price.BlockID] {
			fields["block_prices"] = "Gunakan blok yang memiliki stand aktif pada acara ini."
			break
		}
	}
	for _, quota := range rules.CategoryQuotas {
		if quota.BlockID != "" && !blocks[quota.BlockID] {
			fields["category_quotas"] = "Gunakan blok yang memiliki stand aktif pada acara ini."
			break
		}
	}
	if len(fields) > 0 {
		return target, fields
	}
	return target, nil
}
