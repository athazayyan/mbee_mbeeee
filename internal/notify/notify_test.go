package notify

import (
	"strings"
	"testing"
	"time"
)

func TestRender(t *testing.T) {
	title, body, err := render("event_cancelled", "Pasar Malam Kota", "Kopi Senja")
	if err != nil || title != "Acara dibatalkan" || !strings.Contains(body, "Pasar Malam Kota") || !strings.Contains(body, "Kopi Senja") {
		t.Fatalf("event_cancelled = %q %q %v", title, body, err)
	}
	if _, _, err := render("booking_expired", "A", "B"); err != nil {
		t.Fatalf("booking_expired: %v", err)
	}
	if _, _, err := render("unknown", "A", "B"); err == nil {
		t.Fatal("unknown kind must fail")
	}
	_, body, _ = render("event_cancelled", strings.Repeat("é", 400), strings.Repeat("x", 400))
	if n := len([]rune(body)); n > 500 {
		t.Fatalf("body too long for column: %d runes", n)
	}
}

func TestBackoff(t *testing.T) {
	cases := map[int]time.Duration{0: 30 * time.Second, 1: time.Minute, 3: 4 * time.Minute, 7: time.Hour}
	for attempts, want := range cases {
		if got := backoff(attempts); got != want {
			t.Errorf("backoff(%d) = %s, want %s", attempts, got, want)
		}
	}
	if got := backoff(100); got != time.Hour {
		t.Errorf("backoff(100) = %s", got)
	}
}
