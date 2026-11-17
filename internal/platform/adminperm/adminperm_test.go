package adminperm

import (
	"strings"
	"testing"
)

func TestRedactedHidesSecretsAtAnyDepth(t *testing.T) {
	got, err := Redacted(map[string]any{
		"status":  "verified",
		"nested":  map[string]any{"account_number": "1234567890", "holder": "Budi"},
		"list":    []any{map[string]any{"token": "abc"}, "plain"},
		"api_key": nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	for _, secret := range []string{"1234567890", "abc"} {
		if strings.Contains(text, secret) {
			t.Fatalf("secret %q leaked: %s", secret, text)
		}
	}
	for _, kept := range []string{"verified", "Budi", "plain", hidden} {
		if !strings.Contains(text, kept) {
			t.Fatalf("%q missing: %s", kept, text)
		}
	}
	if null, _ := Redacted(nil); string(null) != "null" {
		t.Fatalf("nil snapshot: %s", null)
	}
}

func TestValid(t *testing.T) {
	if !Valid(FundsManage) || Valid("root") {
		t.Fatal("permission validation")
	}
}
