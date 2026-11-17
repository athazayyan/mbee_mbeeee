package reports

import (
	"bytes"
	"testing"
	"time"
)

func TestRupiah(t *testing.T) {
	cases := map[int64]string{0: "Rp 0", 999: "Rp 999", 1000: "Rp 1.000", 1250000: "Rp 1.250.000", -5000: "-Rp 5.000"}
	for value, want := range cases {
		if got := rupiah(value); got != want {
			t.Errorf("rupiah(%d) = %q, want %q", value, got, want)
		}
	}
}

func TestRenderPDF(t *testing.T) {
	event := "Bazar Uji"
	average, cost, gross, operating := int64(25000), int64(40000), int64(60000), int64(50000)
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	snap := Snapshot{
		BusinessName: "Kopi Senja", EventName: &event, PeriodStart: start, PeriodEnd: start.Add(48 * time.Hour),
		Timezone: "Asia/Jakarta", GeneratedAt: start, CutoffAt: start, Source: "Kasir TenantKu",
		VerificationLabel: "Belum diverifikasi pihak ketiga; bukan skor kredit.", GrossSales: 110000, Discounts: 5000,
		Refunds: 5000, NetSales: 100000, Transactions: 4, AverageTransaction: &average, UnitsNet: 9,
		CostComplete: true, CostOfGoods: &cost, GrossProfit: &gross, Expenses: 10000, OperatingProfit: &operating,
		CashSales: 70000, NonCashSales: 30000,
		Daily:        []DailyTotal{{Date: "2026-11-01", NetSales: 60000, Transactions: 2}, {Date: "2026-11-02", NetSales: 40000, Transactions: 2}},
		TopProducts:  []ProductTotal{{Name: "Es Kopi Susu", Units: 6, NetSales: 72000}},
		Completeness: Completeness{Note: "Semua perangkat tersinkron."},
	}
	body, err := renderPDF(snap, "AB12-CD34")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(body, []byte("%PDF-")) || len(body) < 1000 {
		t.Fatalf("not a pdf: %d bytes", len(body))
	}
	snap.CostComplete, snap.Daily, snap.TopProducts, snap.EventName = false, nil, nil, nil
	if _, err := renderPDF(snap, "AB12-CD34"); err != nil {
		t.Fatal(err)
	}
}
