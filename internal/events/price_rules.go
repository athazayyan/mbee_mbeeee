package events

import (
	"math/big"
	"strings"

	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

const (
	basisPoints = 10_000
	// JSON amounts must be representable exactly by the browser's number type.
	maxRupiah = 1<<53 - 1
)

type slotPrice struct {
	EventSlotID string `json:"event_slot_id"`
	BasePrice   *int64 `json:"base_price"`
}

type occupancyTier struct {
	OccupancyBPS  *int32 `json:"occupancy_bps"`
	MultiplierBPS int32  `json:"multiplier_bps"`
}

type categoryQuota struct {
	CategoryID string `json:"category_id"`
	BlockID    string `json:"block_id,omitempty"`
	Cap        *int32 `json:"cap"`
}

type priceRules struct {
	DefaultBasePrice *int64          `json:"default_base_price,omitempty"`
	SlotPrices       []slotPrice     `json:"slot_prices,omitempty"`
	OccupancyTiers   []occupancyTier `json:"occupancy_tiers,omitempty"`
	Floor            *int64          `json:"floor,omitempty"`
	Cap              *int64          `json:"cap,omitempty"`
	CategoryQuotas   []categoryQuota `json:"category_quotas,omitempty"`
}

func validAmount(value int64) bool { return value >= 0 && value <= maxRupiah }

func (rules *priceRules) validate() fieldErrors {
	fields := make(fieldErrors)
	var highestBase int64
	if rules.DefaultBasePrice != nil {
		highestBase = *rules.DefaultBasePrice
	}
	if rules.DefaultBasePrice == nil && len(rules.SlotPrices) == 0 {
		fields["default_base_price"] = "Isi harga dasar atau harga untuk slot acara."
	}
	for field, amount := range map[string]*int64{
		"default_base_price": rules.DefaultBasePrice, "floor": rules.Floor, "cap": rules.Cap,
	} {
		if amount != nil && !validAmount(*amount) {
			fields[field] = "Masukkan nominal rupiah bulat dalam rentang yang didukung."
		}
	}
	if rules.Floor != nil && rules.Cap != nil && *rules.Floor > *rules.Cap {
		fields["cap"] = "Harga maksimum harus lebih besar atau sama dengan harga minimum."
	}

	slots := make(map[string]bool)
	for index := range rules.SlotPrices {
		price := &rules.SlotPrices[index]
		price.EventSlotID = strings.ToLower(price.EventSlotID)
		if !httpapi.ValidID(price.EventSlotID) || price.BasePrice == nil ||
			!validAmount(*price.BasePrice) || slots[price.EventSlotID] {
			fields["slot_prices"] = "Harga slot harus valid dan setiap slot hanya boleh muncul sekali."
		} else {
			highestBase = max(highestBase, *price.BasePrice)
		}
		slots[price.EventSlotID] = true
	}
	quotas := make(map[string]bool)
	for index := range rules.CategoryQuotas {
		quota := &rules.CategoryQuotas[index]
		quota.CategoryID = strings.ToLower(quota.CategoryID)
		if quota.BlockID != "" {
			fields["category_quotas"] = "Kuota per blok tersedia setelah denah terhubung."
		} else if !httpapi.ValidID(quota.CategoryID) || quota.Cap == nil || *quota.Cap < 0 || quotas[quota.CategoryID] {
			fields["category_quotas"] = "Tetapkan satu kuota yang valid per kategori usaha."
		}
		quotas[quota.CategoryID] = true
	}
	thresholds := make(map[int32]bool)
	highestMultiplier := int32(basisPoints)
	for _, tier := range rules.OccupancyTiers {
		if tier.OccupancyBPS == nil || *tier.OccupancyBPS < 0 || *tier.OccupancyBPS > basisPoints ||
			tier.MultiplierBPS < 1 || thresholds[*tier.OccupancyBPS] {
			fields["occupancy_tiers"] = "Gunakan ambang okupansi 0–10.000 bp yang unik dan pengali positif."
			continue
		}
		thresholds[*tier.OccupancyBPS] = true
		highestMultiplier = max(highestMultiplier, tier.MultiplierBPS)
	}
	if len(fields) == 0 && rules.Cap == nil && roundedRent(highestBase, highestMultiplier).Cmp(big.NewInt(maxRupiah)) > 0 {
		fields["cap"] = "Tetapkan harga maksimum agar hasil pengali tetap dalam rentang rupiah."
	}
	return fields
}

type priceQuote struct {
	BasePrice     int64 `json:"base_price"`
	Rent          int64 `json:"rent"`
	OccupancyBPS  int32 `json:"occupancy_bps"`
	MultiplierBPS int32 `json:"multiplier_bps"`
}

func (rules priceRules) quote(slotID string, confirmed, capacity int32) (priceQuote, error) {
	base := rules.DefaultBasePrice
	for _, slot := range rules.SlotPrices {
		if slot.EventSlotID == slotID {
			base = slot.BasePrice
			break
		}
	}
	if base == nil {
		return priceQuote{}, fieldErrors{"event_slot_id": "Slot ini belum memiliki harga."}
	}
	if capacity < 1 || confirmed < 0 || confirmed > capacity {
		return priceQuote{}, fieldErrors{"confirmed_count": "Jumlah terkonfirmasi harus antara nol dan kapasitas."}
	}
	occupancy := int32(int64(confirmed) * basisPoints / int64(capacity))
	multiplier, applied := int32(basisPoints), int32(-1)
	for _, tier := range rules.OccupancyTiers {
		if *tier.OccupancyBPS <= occupancy && *tier.OccupancyBPS > applied {
			multiplier, applied = tier.MultiplierBPS, *tier.OccupancyBPS
		}
	}

	rent := roundedRent(*base, multiplier)
	if rules.Floor != nil && rent.Cmp(big.NewInt(*rules.Floor)) < 0 {
		rent.SetInt64(*rules.Floor)
	}
	if rules.Cap != nil && rent.Cmp(big.NewInt(*rules.Cap)) > 0 {
		rent.SetInt64(*rules.Cap)
	}
	if rent.Cmp(big.NewInt(maxRupiah)) > 0 {
		return priceQuote{}, fieldErrors{"cap": "Tetapkan harga maksimum agar hasil pengali tetap dalam rentang rupiah."}
	}
	return priceQuote{BasePrice: *base, Rent: rent.Int64(), OccupancyBPS: occupancy, MultiplierBPS: multiplier}, nil
}

func roundedRent(base int64, multiplier int32) *big.Int {
	// Round to the nearest rupiah, with a half rupiah rounded up.
	rent := new(big.Int).Mul(big.NewInt(base), big.NewInt(int64(multiplier)))
	return rent.Add(rent, big.NewInt(basisPoints/2)).Quo(rent, big.NewInt(basisPoints))
}
