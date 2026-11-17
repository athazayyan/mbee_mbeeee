package payment

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// In the sandbox a tester settles their own attempt without paying. It confirms the booking and books the ledger like a
// real settlement, only the owner can do it, and doing it again changes nothing.
func TestSimulateSuccess(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s := migratedStore(t, ctx)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"token":"tok","redirect_url":"https://pay.example/tok"}`))
	}))
	defer provider.Close()
	s.Provider = NewMidtrans(testKey, "sandbox")
	s.Provider.SnapURL = provider.URL

	category := one(t, ctx, s, `SELECT id FROM business_categories ORDER BY id LIMIT 1`)
	user := one(t, ctx, s, `INSERT INTO users (email, password_hash) VALUES ('sim@example.com', 'x') RETURNING id`)
	other := one(t, ctx, s, `INSERT INTO users (email, password_hash) VALUES ('sim2@example.com', 'x') RETURNING id`)
	business := one(t, ctx, s, `INSERT INTO businesses (owner_id, category_id, name) VALUES ($1, $2, 'B') RETURNING id`, user, category)
	if _, err := s.DB.Exec(ctx, `INSERT INTO memberships (user_id, business_id, role) VALUES ($1, $2, 'owner')`, user, business); err != nil {
		t.Fatal(err)
	}
	org := one(t, ctx, s, `INSERT INTO organizations (owner_id, name) VALUES ($1, 'O') RETURNING id`, other)
	event := one(t, ctx, s, `INSERT INTO events (organization_id, created_by, name, slug, status, starts_at, ends_at,
		timezone, city, address, terms) VALUES ($1, $2, 'E', 'e-sim', 'published', now(), now() + interval '2 hours',
		'Asia/Jakarta', 'Banda Aceh', 'St', 'T') RETURNING id`, org, other)
	if _, err := s.DB.Exec(ctx, `INSERT INTO event_layouts (event_id, revision, width, height, cell_meters, updated_by) VALUES ($1, 1, 8, 8, 1, $2)`, event, other); err != nil {
		t.Fatal(err)
	}
	slot := one(t, ctx, s, `INSERT INTO event_slots (event_id, code, starts_at, ends_at) SELECT id, 'whole', starts_at, ends_at FROM events WHERE id = $1 RETURNING id`, event)
	if _, err := s.DB.Exec(ctx, `INSERT INTO event_pricing_versions (event_id, revision, created_by, rules, effective_at)
		VALUES ($1, 1, $2, '{"default_base_price":30000}', now())`, event, other); err != nil {
		t.Fatal(err)
	}
	booking := seedBooking(t, ctx, s, user, business, category, event, slot, "00000000-0000-4000-8000-000000000002")
	attempt, _, err := s.CreateAttempt(ctx, user, "sim@example.com", booking, "k1", Input{Method: "snap"})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.SimulateSuccess(ctx, user, attempt.ID); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("simulation must be off by default: %v", err)
	}
	s.Simulation = true
	if err := s.SimulateSuccess(ctx, other, attempt.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("someone else simulated the payment: %v", err)
	}
	if err := s.SimulateSuccess(ctx, user, attempt.ID); err != nil {
		t.Fatal(err)
	}
	if one(t, ctx, s, `SELECT status FROM payment_attempts WHERE id = $1`, attempt.ID) != "paid" ||
		one(t, ctx, s, `SELECT status FROM bookings WHERE id = $1`, booking) != "confirmed" ||
		one(t, ctx, s, `SELECT status FROM allocations WHERE booking_id = $1`, booking) != "reserved" ||
		count(t, ctx, s, `SELECT count(*) FROM ledger_transactions WHERE kind = 'payment_received' AND ref_id = $1`, attempt.ID) != 1 {
		t.Fatal("the simulated payment did not confirm the booking and book the ledger")
	}
	// Again: nothing changes, no duplicate exception or ledger entry.
	if err := s.SimulateSuccess(ctx, user, attempt.ID); err != nil {
		t.Fatal(err)
	}
	if count(t, ctx, s, `SELECT count(*) FROM payment_exceptions`) != 0 ||
		count(t, ctx, s, `SELECT count(*) FROM ledger_transactions`) != 1 {
		t.Fatal("a repeat changed the books")
	}
}
