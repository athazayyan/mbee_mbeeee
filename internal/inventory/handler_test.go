package inventory

import (
	"encoding/json"
	"testing"
)

func decodeProduct(t *testing.T, body string) productInput {
	t.Helper()
	var input productInput
	if err := json.Unmarshal([]byte(body), &input); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return input
}

func TestValidateProductCreate(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		field string
	}{
		{"valid", `{"name":" Kopi Susu ","price":18000,"sku":"KS-01","initial_stock":20}`, ""},
		{"empty sku becomes null", `{"name":"Teh","price":5000,"sku":"  "}`, ""},
		{"missing name", `{"price":1000}`, "name"},
		{"null name", `{"name":null,"price":1000}`, "name"},
		{"multiline name", `{"name":"a\nb","price":1000}`, "name"},
		{"missing price", `{"name":"Teh"}`, "price"},
		{"negative price", `{"name":"Teh","price":-1}`, "price"},
		{"price above safe integer", `{"name":"Teh","price":9007199254740992}`, "price"},
		{"negative cost", `{"name":"Teh","price":1,"cost":-5}`, "cost"},
		{"bad sku", `{"name":"Teh","price":1,"sku":"a b"}`, "sku"},
		{"bad category", `{"name":"Teh","price":1,"category_id":"x"}`, "category_id"},
		{"photo id rusak", `{"name":"Teh","price":1,"photo_upload_id":"bukan-uuid"}`, "photo_upload_id"},
		{"negative initial stock", `{"name":"Teh","price":1,"initial_stock":-1}`, "initial_stock"},
		{"too much initial stock", `{"name":"Teh","price":1,"initial_stock":1000001}`, "initial_stock"},
		{"archived on create", `{"name":"Teh","price":1,"archived":true}`, "archived"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := decodeProduct(t, tc.body)
			fields := validateProduct(&input, true)
			if tc.field == "" {
				if fields != nil {
					t.Fatalf("unexpected errors %v", fields)
				}
				return
			}
			if _, ok := fields[tc.field]; !ok {
				t.Fatalf("want error on %s, got %v", tc.field, fields)
			}
		})
	}
}

func TestValidateProductNormalises(t *testing.T) {
	input := decodeProduct(t, `{"name":"  Roti  ","price":1,"sku":" ab-1 ","category_id":"8C7C3B0E-8F3A-4B8E-9D55-1A2B3C4D5E6F"}`)
	if fields := validateProduct(&input, true); fields != nil {
		t.Fatal(fields)
	}
	if *input.Name.Value != "Roti" || *input.SKU.Value != "ab-1" ||
		*input.CategoryID.Value != "8c7c3b0e-8f3a-4b8e-9d55-1a2b3c4d5e6f" {
		t.Fatalf("not normalised: %q %q %q", *input.Name.Value, *input.SKU.Value, *input.CategoryID.Value)
	}
}

func TestValidateProductUpdate(t *testing.T) {
	cases := []struct {
		body  string
		field string
	}{
		{`{"price":2000}`, ""},
		{`{"archived":true}`, ""},
		{`{"sku":null,"cost":null,"category_id":null}`, ""},
		{`{}`, "name"},
		{`{"price":null}`, "price"},
		{`{"archived":null}`, "archived"},
		{`{"initial_stock":5}`, "initial_stock"},
	}
	for _, tc := range cases {
		input := decodeProduct(t, tc.body)
		fields := validateProduct(&input, false)
		if tc.field == "" && fields != nil {
			t.Errorf("%s: unexpected %v", tc.body, fields)
		}
		if tc.field != "" {
			if _, ok := fields[tc.field]; !ok {
				t.Errorf("%s: want error on %s, got %v", tc.body, tc.field, fields)
			}
		}
	}
}

func TestValidateStock(t *testing.T) {
	note := "  "
	cases := []struct {
		input stockInput
		field string
	}{
		{stockInput{Delta: 10, Reason: "restock"}, ""},
		{stockInput{Delta: -3, Reason: "correction"}, ""},
		{stockInput{Delta: 1, Reason: "restock", Note: &note}, ""},
		{stockInput{Delta: -1, Reason: "restock"}, "delta"},
		{stockInput{Delta: 100001, Reason: "restock"}, "delta"},
		{stockInput{Delta: 0, Reason: "correction"}, "delta"},
		{stockInput{Delta: -100001, Reason: "correction"}, "delta"},
		{stockInput{Delta: 1, Reason: "sale"}, "reason"},
		{stockInput{Delta: 1, Reason: ""}, "reason"},
	}
	for _, tc := range cases {
		input := tc.input
		fields := validateStock(&input)
		if tc.field == "" && fields != nil {
			t.Errorf("%+v: unexpected %v", tc.input, fields)
		}
		if tc.field != "" {
			if _, ok := fields[tc.field]; !ok {
				t.Errorf("%+v: want error on %s, got %v", tc.input, tc.field, fields)
			}
		}
	}
	if input := (stockInput{Delta: 1, Reason: "restock", Note: &note}); validateStock(&input) == nil && input.Note != nil {
		t.Error("blank note should become nil")
	}
}
