package reports

import (
	"testing"
	"time"
)

func cost(value int64) *int64 { return &value }

func TestAggregateFollowsReportDefinitions(t *testing.T) {
	sales := []saleRow{
		// 2 kopi 18rb + 1 roti 10rb diskon item 2rb, potongan transaksi 4rb → total 40rb; retur 1 kopi 16.363.
		{ID: "a", Day: "2026-12-01", Total: 40000, Discount: 4000, Method: "cash", Refunded: 16363, Returned: true},
		// Transaksi non-tunai yang di-void penuh.
		{ID: "b", Day: "2026-12-01", Total: 15000, Method: "external", Refunded: 15000, Voided: true},
		{ID: "c", Day: "2026-12-02", Total: 20000, Method: "external"},
	}
	items := []itemRow{
		{SaleID: "a", ProductID: "kopi", Name: "Kopi", Quantity: 2, UnitPrice: 18000, UnitCost: cost(8000), Subtotal: 36000, Returned: 1},
		{SaleID: "a", ProductID: "roti", Name: "Roti", Quantity: 1, UnitPrice: 10000, UnitCost: cost(6000), Discount: 2000, Subtotal: 8000},
		{SaleID: "b", ProductID: "teh", Name: "Teh", Quantity: 3, UnitPrice: 5000, UnitCost: cost(2000), Subtotal: 15000, Returned: 3},
		{SaleID: "c", ProductID: "kopi", Name: "Kopi", Quantity: 1, UnitPrice: 20000, UnitCost: cost(8000), Subtotal: 20000},
	}
	s := aggregate(sales, items, 5000)
	checks := []struct {
		name      string
		got, want int64
	}{
		{"gross", s.GrossSales, 36000 + 10000 + 15000 + 20000},
		{"discounts", s.Discounts, 4000 + 2000},
		{"refunds", s.Refunds, 16363 + 15000},
		{"net", s.NetSales, (40000 - 16363) + 0 + 20000},
		{"transactions excludes fully voided", s.Transactions, 2},
		{"voided", s.VoidedTransactions, 1},
		{"returns", s.ReturnTransactions, 1},
		{"units sold", s.UnitsSold, 7},
		{"units net", s.UnitsNet, 3},
		{"cash", s.CashSales, 40000 - 16363},
		{"non cash", s.NonCashSales, 20000},
		{"expenses", s.Expenses, 5000},
	}
	for _, check := range checks {
		if check.got != check.want {
			t.Errorf("%s: want %d, got %d", check.name, check.want, check.got)
		}
	}
	if s.AverageTransaction == nil || *s.AverageTransaction != s.NetSales/2 {
		t.Errorf("average: %v", s.AverageTransaction)
	}
	wantCost := int64(8000 + 6000 + 8000)
	if !s.CostComplete || *s.CostOfGoods != wantCost || *s.GrossProfit != s.NetSales-wantCost || *s.OperatingProfit != s.NetSales-wantCost-5000 {
		t.Errorf("profit: complete=%v cost=%v gross=%v op=%v", s.CostComplete, *s.CostOfGoods, *s.GrossProfit, *s.OperatingProfit)
	}
	if len(s.Daily) != 2 || s.Daily[0].Date != "2026-12-01" || s.Daily[0].Transactions != 1 || s.Daily[1].NetSales != 20000 {
		t.Errorf("daily: %+v", s.Daily)
	}
	if len(s.TopProducts) != 2 || s.TopProducts[0].Name != "Kopi" || s.TopProducts[0].Units != 2 {
		t.Errorf("top products: %+v", s.TopProducts)
	}
}

func TestAggregateHidesProfitWhenCostMissing(t *testing.T) {
	s := aggregate([]saleRow{{ID: "a", Day: "d", Total: 10000, Method: "cash"}},
		[]itemRow{{SaleID: "a", ProductID: "p", Name: "P", Quantity: 1, UnitPrice: 10000, Subtotal: 10000}}, 0)
	if s.CostComplete || s.GrossProfit != nil || s.OperatingProfit != nil {
		t.Fatalf("profit must be hidden: %+v", s)
	}
	// Barang tanpa HPP yang sudah diretur penuh tidak membuat laba tersembunyi.
	s = aggregate([]saleRow{{ID: "a", Day: "d", Total: 10000, Method: "cash", Refunded: 10000, Voided: true}},
		[]itemRow{{SaleID: "a", ProductID: "p", Name: "P", Quantity: 1, UnitPrice: 10000, Subtotal: 10000, Returned: 1}}, 0)
	if !s.CostComplete || s.AverageTransaction != nil || s.Transactions != 0 {
		t.Fatalf("fully voided sale: %+v", s)
	}
}

func TestValidateReport(t *testing.T) {
	now := time.Date(2026, 12, 10, 0, 0, 0, 0, time.UTC)
	ok := reportInput{PeriodStart: now.Add(-7 * 24 * time.Hour), PeriodEnd: now}
	if fields := validateReport(&ok, now); fields != nil || ok.Timezone != "Asia/Jakarta" {
		t.Fatalf("unexpected %v %s", fields, ok.Timezone)
	}
	bad := []reportInput{
		{PeriodStart: now, PeriodEnd: now},
		{PeriodStart: now.Add(-400 * 24 * time.Hour), PeriodEnd: now},
		{PeriodStart: now.Add(time.Hour), PeriodEnd: now.Add(2 * time.Hour)},
		{PeriodStart: now.Add(-time.Hour), PeriodEnd: now, Timezone: "WIB"},
	}
	for index, input := range bad {
		if validateReport(&input, now) == nil {
			t.Errorf("case %d should fail", index)
		}
	}
}

func TestShareExpiry(t *testing.T) {
	now := time.Date(2026, 12, 10, 0, 0, 0, 0, time.UTC)
	if got, fields := shareExpiry(shareInput{}, now); fields != nil || !got.Equal(now.Add(7*24*time.Hour)) {
		t.Errorf("default: %v %v", got, fields)
	}
	tooLong := now.Add(31 * 24 * time.Hour)
	if _, fields := shareExpiry(shareInput{ExpiresAt: &tooLong}, now); fields == nil {
		t.Error("31 days must be rejected")
	}
	full := "full"
	if _, fields := shareExpiry(shareInput{Scope: &full}, now); fields == nil {
		t.Error("unknown scope must be rejected")
	}
	if code, err := verificationCode(); err != nil || len(code) != 9 || code[4] != '-' {
		t.Errorf("verification code %q %v", code, err)
	}
	if string(hashToken("a")) == string(hashToken("b")) {
		t.Error("token hashes must differ")
	}
}
