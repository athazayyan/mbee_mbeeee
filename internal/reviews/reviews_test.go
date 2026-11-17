package reviews

import (
	"strings"
	"testing"
	"time"
)

func TestEligibility(t *testing.T) {
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	past, future := now.Add(-time.Hour), now.Add(time.Hour)
	cases := []struct {
		name, booking, event string
		ends                 time.Time
		want                 string
	}{
		{"selesai", "confirmed", "published", past, ""},
		{"status completed walau jam belum lewat", "confirmed", "completed", future, ""},
		{"belum berakhir", "confirmed", "live", future, "event_not_finished"},
		{"belum dibayar", "payment_pending", "completed", past, "booking_not_confirmed"},
		{"hold kedaluwarsa", "expired", "completed", past, "booking_not_confirmed"},
		{"acara dibatalkan", "confirmed", "cancelled", past, "event_cancelled"},
	}
	for _, tc := range cases {
		if got := eligibility(tc.booking, tc.event, tc.ends, now); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestNormalizeReview(t *testing.T) {
	blank := "   "
	padded := "  Penyelenggara rapi.  "
	input := reviewInput{Rating: 5, Comment: &padded}
	if fields := normalizeReview(&input); fields != nil || *input.Comment != "Penyelenggara rapi." {
		t.Fatalf("padded: %v %q", fields, *input.Comment)
	}
	input = reviewInput{Rating: 4, Comment: &blank}
	if fields := normalizeReview(&input); fields != nil || input.Comment != nil {
		t.Fatalf("blank comment must become nil: %v", fields)
	}
	for _, rating := range []int{0, 6, -1} {
		if fields := normalizeReview(&reviewInput{Rating: rating}); fields["rating"] == "" {
			t.Errorf("rating %d must fail", rating)
		}
	}
	long := strings.Repeat("a", maxComment+1)
	if fields := normalizeReview(&reviewInput{Rating: 3, Comment: &long}); fields["comment"] == "" {
		t.Error("long comment must fail")
	}
}

func TestSameText(t *testing.T) {
	a, b := "x", "x"
	if !sameText(nil, nil) || sameText(&a, nil) || !sameText(&a, &b) {
		t.Fatal("sameText mismatch")
	}
}
