package inventory

import (
	"testing"
	"time"
)

func TestComputeRefund(t *testing.T) {
	// Transaksi: 2 kopi @18.000 (36.000) + 1 roti 10.000 diskon 2.000 (8.000); potongan transaksi 4.000 → total 40.000.
	sale := func(kopi, roti int64) []refundLine {
		return []refundLine{
			{quantity: 2, subtotal: 36000, remaining: 2, take: kopi},
			{quantity: 1, subtotal: 8000, remaining: 1, take: roti},
		}
	}
	cases := []struct {
		name     string
		lines    []refundLine
		refunded int64
		want     int64
	}{
		{"void semua", sale(2, 1), 0, 40000},
		{"retur 1 kopi, potongan dibagi proporsional", sale(1, 0), 0, 18000 * 40000 / 44000},
		{"retur roti saja", sale(0, 1), 0, 8000 * 40000 / 44000},
		{"sisa terakhir menutup tepat total", []refundLine{
			{quantity: 2, subtotal: 36000, remaining: 1, take: 1},
			{quantity: 1, subtotal: 8000, remaining: 0, take: 0},
		}, 23636, 40000 - 23636},
	}
	for _, tc := range cases {
		if got := computeRefund(tc.lines, 40000, tc.refunded); got != tc.want {
			t.Errorf("%s: want %d, got %d", tc.name, tc.want, got)
		}
	}
	// Tidak pernah melebihi sisa total, walau proporsinya lebih besar.
	if got := computeRefund([]refundLine{{quantity: 3, subtotal: 30000, remaining: 3, take: 2}}, 30000, 29000); got != 1000 {
		t.Errorf("cap: want 1000, got %d", got)
	}
	if got := computeRefund([]refundLine{{quantity: 1, subtotal: 0, remaining: 1, take: 1}}, 0, 0); got != 0 {
		t.Errorf("free sale: want 0, got %d", got)
	}
	// Retur satu per satu hingga habis selalu berjumlah tepat total.
	lines := []refundLine{{quantity: 3, subtotal: 10000, remaining: 3}}
	var refunded int64
	for step := 0; step < 3; step++ {
		lines[0].take = 1
		refunded += computeRefund(lines, 9500, refunded)
		lines[0].remaining--
	}
	if refunded != 9500 {
		t.Errorf("cumulative returns: want 9500, got %d", refunded)
	}
}

func TestValidateAdjustment(t *testing.T) {
	item := "70707070-7070-4070-8070-000000000070"
	other := "80808080-8080-4080-8080-000000000080"
	cases := []struct {
		name  string
		input adjustmentInput
		field string
	}{
		{"void", adjustmentInput{Type: "void", Reason: "Salah input"}, ""},
		{"return", adjustmentInput{Type: "return", Reason: "Rusak", Items: []adjustmentItemInput{{item, 1}, {other, 2}}}, ""},
		{"void with items", adjustmentInput{Type: "void", Reason: "x", Items: []adjustmentItemInput{{item, 1}}}, "items"},
		{"return without items", adjustmentInput{Type: "return", Reason: "x"}, "items"},
		{"duplicate item", adjustmentInput{Type: "return", Reason: "x", Items: []adjustmentItemInput{{item, 1}, {item, 1}}}, "items"},
		{"zero quantity", adjustmentInput{Type: "return", Reason: "x", Items: []adjustmentItemInput{{item, 0}}}, "items"},
		{"bad id", adjustmentInput{Type: "return", Reason: "x", Items: []adjustmentItemInput{{"x", 1}}}, "items"},
		{"blank reason", adjustmentInput{Type: "void", Reason: "  "}, "reason"},
		{"unknown type", adjustmentInput{Type: "refund", Reason: "x"}, "type"},
	}
	for _, tc := range cases {
		input := tc.input
		fields := validateAdjustment(&input)
		if tc.field == "" && fields != nil {
			t.Errorf("%s: unexpected %v", tc.name, fields)
		}
		if tc.field != "" && fields[tc.field] == "" {
			t.Errorf("%s: want error on %s, got %v", tc.name, tc.field, fields)
		}
	}
	// Urutan barang tidak memengaruhi sidik idempotensi.
	a := adjustmentInput{Type: "return", Reason: "r", Items: []adjustmentItemInput{{other, 1}, {item, 2}}}
	b := adjustmentInput{Type: "return", Reason: "r", Items: []adjustmentItemInput{{item, 2}, {other, 1}}}
	validateAdjustment(&a)
	validateAdjustment(&b)
	if string(adjustmentDigest("s", a)) != string(adjustmentDigest("s", b)) {
		t.Error("item order must not change the digest")
	}
}

func TestValidateExpense(t *testing.T) {
	now := time.Date(2026, 12, 1, 10, 0, 0, 0, time.UTC)
	note := "  Bensin  "
	ok := expenseInput{Amount: 50000, Category: "transportasi", OccurredAt: now.Add(-time.Hour), Note: &note}
	if fields := validateExpense(&ok, now); fields != nil || *ok.Note != "Bensin" {
		t.Fatalf("unexpected %v", fields)
	}
	bad := expenseInput{Amount: 0, Category: "jajan", OccurredAt: now.Add(48 * time.Hour)}
	fields := validateExpense(&bad, now)
	for _, key := range []string{"amount", "category", "occurred_at"} {
		if fields[key] == "" {
			t.Errorf("want error on %s, got %v", key, fields)
		}
	}
}
