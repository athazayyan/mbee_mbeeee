package inventory

import (
	"bytes"
	"testing"
	"time"
)

var saleNow = time.Date(2026, 12, 1, 10, 0, 0, 0, time.UTC)

func validSale() saleInput {
	return saleInput{
		SaleID:          "10101010-1010-4010-8010-000000000010",
		DeviceID:        "20202020-2020-4020-8020-000000000020",
		BusinessID:      "30303030-3030-4030-8030-000000000030",
		SessionID:       "40404040-4040-4040-8040-000000000040",
		ClientCreatedAt: saleNow.Add(-time.Hour),
		SchemaVersion:   1,
		PayloadHash:     "abc123",
		Currency:        "IDR",
		Method:          "cash",
		Items: []saleItemInput{
			{ProductID: "50505050-5050-4050-8050-000000000050", ProductName: " Kopi ", Quantity: 2, UnitPrice: 18000, Subtotal: 36000},
			{ProductID: "60606060-6060-4060-8060-000000000060", ProductName: "Roti", Quantity: 1, UnitPrice: 10000, Discount: 2000, Subtotal: 8000},
		},
		Discount: 4000,
		Total:    40000,
	}
}

func TestValidateSale(t *testing.T) {
	cases := []struct {
		name   string
		change func(*saleInput)
		want   rejection
	}{
		{"valid", func(*saleInput) {}, ""},
		{"external payment", func(s *saleInput) { s.Method = "external" }, ""},
		{"bad sale id", func(s *saleInput) { s.SaleID = "x" }, "INVALID_SALE"},
		{"bad event id", func(s *saleInput) { bad := "x"; s.EventID = &bad }, "INVALID_SALE"},
		{"schema version", func(s *saleInput) { s.SchemaVersion = 2 }, "UNSUPPORTED_SCHEMA_VERSION"},
		{"empty hash", func(s *saleInput) { s.PayloadHash = "" }, "INVALID_SALE"},
		{"currency", func(s *saleInput) { s.Currency = "USD" }, "INVALID_SALE"},
		{"method", func(s *saleInput) { s.Method = "qris" }, "INVALID_SALE"},
		{"zero time", func(s *saleInput) { s.ClientCreatedAt = time.Time{} }, "INVALID_TIMESTAMP"},
		{"far future", func(s *saleInput) { s.ClientCreatedAt = saleNow.Add(25 * time.Hour) }, "INVALID_TIMESTAMP"},
		{"slightly ahead clock", func(s *saleInput) { s.ClientCreatedAt = saleNow.Add(time.Hour) }, ""},
		{"no items", func(s *saleInput) { s.Items = nil }, "INVALID_ITEMS"},
		{"zero quantity", func(s *saleInput) { s.Items[0].Quantity = 0 }, "INVALID_ITEMS"},
		{"blank product name", func(s *saleInput) { s.Items[0].ProductName = "  " }, "INVALID_ITEMS"},
		{"negative cost", func(s *saleInput) { cost := int64(-1); s.Items[0].UnitCost = &cost }, "INVALID_ITEMS"},
		{"wrong subtotal", func(s *saleInput) { s.Items[0].Subtotal = 35000 }, "INVALID_TOTAL"},
		{"item discount above gross", func(s *saleInput) { s.Items[1].Discount = 20000; s.Items[1].Subtotal = -10000 }, "INVALID_TOTAL"},
		{"wrong total", func(s *saleInput) { s.Total = 44000 }, "INVALID_TOTAL"},
		{"sale discount above sum", func(s *saleInput) { s.Discount = 50000; s.Total = 0 }, "INVALID_TOTAL"},
		{"price overflow", func(s *saleInput) { s.Items[0].UnitPrice = maxRupiah; s.Items[0].Quantity = 2 }, "INVALID_TOTAL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sale := validSale()
			tc.change(&sale)
			if got := validateSale(&sale, saleNow); got != tc.want {
				t.Fatalf("want %q, got %q", tc.want, got)
			}
		})
	}
}

func TestValidateSaleNormalises(t *testing.T) {
	sale := validSale()
	sale.SaleID = "10101010-1010-4010-8010-00000000001A"
	sale.ClientCreatedAt = time.Date(2026, 12, 1, 15, 0, 0, 0, time.FixedZone("WIB", 7*3600))
	if code := validateSale(&sale, saleNow); code != "" {
		t.Fatal(code)
	}
	if sale.SaleID != "10101010-1010-4010-8010-00000000001a" || sale.Items[0].ProductName != "Kopi" ||
		sale.ClientCreatedAt.Location() != time.UTC {
		t.Fatalf("not normalised: %+v", sale)
	}
}

func TestSaleDigestDetectsChanges(t *testing.T) {
	first, second := validSale(), validSale()
	if !bytes.Equal(saleDigest(first), saleDigest(second)) {
		t.Fatal("identical sales must share a digest")
	}
	second.Items[0].Quantity = 3
	if bytes.Equal(saleDigest(first), saleDigest(second)) {
		t.Fatal("changed quantity must change the digest")
	}
}

func TestValidateSessionInput(t *testing.T) {
	input := posSessionInput{DeviceID: "20202020-2020-4020-8020-00000000002A"}
	if fields := validateSessionInput(&input); fields != nil || input.DeviceID != "20202020-2020-4020-8020-00000000002a" {
		t.Fatalf("unexpected %v %s", fields, input.DeviceID)
	}
	bad := "x"
	input = posSessionInput{DeviceID: "nope", EventID: &bad}
	fields := validateSessionInput(&input)
	if fields["device_id"] == "" || fields["event_id"] == "" {
		t.Fatalf("want both errors, got %v", fields)
	}
}
