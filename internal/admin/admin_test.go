package admin

import "testing"

func testCatalog() PlanCatalog {
	plans := []Plan{
		{"mini", "Mini", 20, 180000, 0}, {"basic", "Basic", 30, 250000, 0}, {"standard", "Standard", 40, 300000, 0},
		{"plus", "Plus", 50, 350000, 0}, {"medium", "Medium", 75, 475000, 0}, {"large", "Large", 100, 600000, 0},
	}
	return PlanCatalog{CustomPricePerTenant: 5000, Plans: plans}
}

func TestPerTenantMatchesPRD(t *testing.T) {
	// Kolom "biaya rata-rata per tenant saat penuh" pada PRD 9.1.
	want := map[string]int64{"mini": 9000, "basic": 8333, "standard": 7500, "plus": 7000, "medium": 6333, "large": 6000}
	for _, plan := range testCatalog().Plans {
		if got := perTenant(plan.Price, plan.Capacity); got != want[plan.Code] {
			t.Errorf("%s: want %d, got %d", plan.Code, want[plan.Code], got)
		}
	}
}

func TestQuotePlan(t *testing.T) {
	cases := []struct {
		capacity int64
		code     string
		price    int64
	}{
		{1, "mini", 180000},
		{20, "mini", 180000},
		{21, "basic", 250000},
		{40, "standard", 300000},
		{76, "large", 600000},
		{100, "large", 600000},
		{101, "custom", 605000},
		{150, "custom", 850000},
	}
	for _, tc := range cases {
		got, ok := quotePlan(testCatalog(), tc.capacity)
		if !ok || got.Code != tc.code || got.Price != tc.price {
			t.Errorf("capacity %d: want %s %d, got %+v", tc.capacity, tc.code, tc.price, got)
		}
	}
	if _, ok := quotePlan(testCatalog(), 0); ok {
		t.Error("capacity 0 must not quote")
	}
	if got, _ := quotePlan(testCatalog(), 150); got.Capacity != 150 || got.PerTenant != 5667 {
		t.Errorf("custom per tenant: %+v", got)
	}
}

func TestValidateVerification(t *testing.T) {
	ok := verificationInput{Status: "verified", Reason: "  Dokumen legalitas lengkap  "}
	if fields := validateVerification(&ok); fields != nil || ok.Reason != "Dokumen legalitas lengkap" {
		t.Fatalf("unexpected %v %q", fields, ok.Reason)
	}
	bad := verificationInput{Status: "approved", Reason: "   "}
	fields := validateVerification(&bad)
	if fields["status"] == "" || fields["reason"] == "" {
		t.Fatalf("want both errors, got %v", fields)
	}
}
