package admin

import "time"

// Plan adalah satu paket acara pada versi harga yang berlaku (PRD 9.1).
type Plan struct {
	Code      string `json:"code"`
	Name      string `json:"name"`
	Capacity  int64  `json:"capacity"`
	Price     int64  `json:"price"`
	PerTenant int64  `json:"price_per_tenant"`
}

type PlanCatalog struct {
	VersionID            int       `json:"version_id"`
	EffectiveAt          time.Time `json:"effective_at"`
	CustomPricePerTenant int64     `json:"custom_price_per_tenant"`
	Plans                []Plan    `json:"plans"`
}

type PlanQuote struct {
	Code      string `json:"code"`
	Name      string `json:"name"`
	Capacity  int64  `json:"capacity"`
	Price     int64  `json:"price"`
	PerTenant int64  `json:"price_per_tenant"`
}

// perTenant membulatkan harga paket per tenant saat penuh ke rupiah terdekat.
func perTenant(price, capacity int64) int64 {
	if capacity <= 0 {
		return 0
	}
	return (price + capacity/2) / capacity
}

// quotePlan memilih paket terkecil yang muat untuk kapasitas tenant. Di atas paket terbesar,
// paket Custom memakai harga paket terbesar ditambah tarif per tenant tambahan.
func quotePlan(catalog PlanCatalog, capacity int64) (PlanQuote, bool) {
	if capacity < 1 || len(catalog.Plans) == 0 {
		return PlanQuote{}, false
	}
	largest := catalog.Plans[0]
	for _, plan := range catalog.Plans {
		if plan.Capacity >= capacity {
			return PlanQuote{plan.Code, plan.Name, plan.Capacity, plan.Price, perTenant(plan.Price, plan.Capacity)}, true
		}
		if plan.Capacity > largest.Capacity {
			largest = plan
		}
	}
	price := largest.Price + (capacity-largest.Capacity)*catalog.CustomPricePerTenant
	return PlanQuote{"custom", "Custom", capacity, price, perTenant(price, capacity)}, true
}
