package reviews

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"kitakarsa.com/tenantku/backend/internal/platform/testdb"
)

// E15-06: kelayakan ulasan dan satu ulasan per pihak per booking.
func TestReviewIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool := testdb.Migrated(t, ctx, "reviews_test")
	s := Store{DB: pool}
	five := reviewInput{Rating: 5}

	for _, tc := range []struct {
		status string
		ended  bool
		reason string
	}{
		{"payment_pending", true, "booking_not_confirmed"},
		{"expired", true, "booking_not_confirmed"},
		{"confirmed", false, "event_not_finished"},
	} {
		w := testdb.NewWorld(t, ctx, pool, tc.status, tc.ended)
		var ineligible notEligible
		if _, err := s.create(ctx, w.Tenant, w.Booking, five); !errors.As(err, &ineligible) || ineligible.reason != tc.reason {
			t.Fatalf("%s ended=%v: %v", tc.status, tc.ended, err)
		}
	}

	w := testdb.NewWorld(t, ctx, pool, "confirmed", true)
	stranger := testdb.User(t, ctx, pool, "luar")
	if _, err := s.create(ctx, stranger, w.Booking, five); !errors.Is(err, errNotFound) {
		t.Fatalf("stranger reviewed booking: %v", err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 5)
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.create(ctx, w.Tenant, w.Booking, five)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	created := 0
	for err := range results {
		switch {
		case err == nil:
			created++
		case !errors.Is(err, errExists):
			t.Fatalf("parallel review: %v", err)
		}
	}
	if created != 1 {
		t.Fatalf("tenant created %d reviews, want 1", created)
	}
	if _, err := s.create(ctx, w.Organizer, w.Booking, reviewInput{Rating: 4}); err != nil {
		t.Fatalf("organizer review: %v", err)
	}
	state, err := s.forBooking(ctx, w.Tenant, w.Booking)
	if err != nil || state.Mine == nil || state.Counterpart == nil || state.Counterpart.AuthorSide != "organization" {
		t.Fatalf("booking reviews: %+v %v", state, err)
	}
	public, err := s.organizer(ctx, w.Organization)
	if err != nil || public.Count != 1 {
		t.Fatalf("public reviews must only include tenant reviews: %+v %v", public, err)
	}
}
