package messaging

import (
	"strings"
	"testing"
)

const validID = "6f1f9d1e-2b7c-4d5e-9a3b-1c2d3e4f5a6b"

func TestValidateMessage(t *testing.T) {
	cases := []struct {
		name  string
		input messageInput
		field string
	}{
		{"valid", messageInput{validID, "Halo, stand saya di blok A?"}, ""},
		{"multiline", messageInput{validID, "Baris satu\nBaris dua"}, ""},
		{"blank", messageInput{validID, "   \n"}, "text"},
		{"control", messageInput{validID, "halo\x00"}, "text"},
		{"too long", messageInput{validID, strings.Repeat("a", maxText+1)}, "text"},
		{"max runes", messageInput{validID, strings.Repeat("é", maxText)}, ""},
		{"bad id", messageInput{"bukan-uuid", "halo"}, "client_message_id"},
	}
	for _, tc := range cases {
		fields := validateMessage(tc.input)
		if tc.field == "" && fields != nil {
			t.Errorf("%s: unexpected %v", tc.name, fields)
		}
		if tc.field != "" && fields[tc.field] == "" {
			t.Errorf("%s: want error on %s, got %v", tc.name, tc.field, fields)
		}
	}
}

func TestValidateReport(t *testing.T) {
	note := "menawarkan transfer di luar platform"
	blank := "  "
	if fields := validateReport(reportInput{Reason: "fraud"}); fields != nil {
		t.Errorf("fraud: %v", fields)
	}
	if fields := validateReport(reportInput{Reason: "other", Note: &note}); fields != nil {
		t.Errorf("other with note: %v", fields)
	}
	if fields := validateReport(reportInput{Reason: "other", Note: &blank}); fields["note"] == "" {
		t.Error("other without note must fail")
	}
	if fields := validateReport(reportInput{Reason: "marah"}); fields["reason"] == "" {
		t.Error("unknown reason must fail")
	}
}

func TestValidateResolve(t *testing.T) {
	if fields := validateResolve(resolveInput{Action: "hide", Reason: "Ujaran kasar"}); fields != nil {
		t.Errorf("hide: %v", fields)
	}
	if fields := validateResolve(resolveInput{Action: "delete", Reason: "x"}); fields["action"] == "" {
		t.Error("unknown action must fail")
	}
	if fields := validateResolve(resolveInput{Action: "dismiss"}); fields["reason"] == "" {
		t.Error("empty reason must fail")
	}
}
