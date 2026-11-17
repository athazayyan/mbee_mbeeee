package payment

import (
	"context"
	"crypto/rand"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	bookingstore "kitakarsa.com/tenantku/backend/internal/booking"
)

const testKey = "Mid-server-test"

type snapTransport func(*http.Request) (*http.Response, error)

func (transport snapTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return transport(req)
}

func migratedStore(t *testing.T, ctx context.Context) Store {
	t.Helper()
	databaseURL := os.Getenv("TENANTKU_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set TENANTKU_TEST_DATABASE_URL to run the PostgreSQL payment test")
	}
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	schema := pgx.Identifier{"payment_test_" + strings.ToLower(rand.Text())}.Sanitize()
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		defer conn.Close(cleanup)
		conn.Exec(cleanup, fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", schema))
	})
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	files, err := os.ReadDir("../../migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if !strings.HasSuffix(f.Name(), ".up.sql") {
			continue
		}
		// This deployment function is bound to public, while the fixture uses its own schema.
		if f.Name() == "00013_cancel_unpaid_event_bookings.up.sql" {
			continue
		}
		sql, err := os.ReadFile(filepath.Join("../../migrations", f.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("%s: %v", f.Name(), err)
		}
	}
	return Store{DB: pool}
}

func one(t *testing.T, ctx context.Context, s Store, query string, args ...any) string {
	t.Helper()
	var v string
	if err := s.DB.QueryRow(ctx, query, args...).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func count(t *testing.T, ctx context.Context, s Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.DB.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func seedBooking(t *testing.T, ctx context.Context, s Store, user, business, category, event, slot string, stand string) string {
	t.Helper()
	if _, err := s.DB.Exec(ctx, `INSERT INTO stands (id, event_id, code, x, y, width, height)
		VALUES ($1::uuid, $2, right($1::uuid::text, 4), right($1::uuid::text, 1)::int, 0, 1, 1)`, stand, event); err != nil {
		t.Fatal(err)
	}
	id := one(t, ctx, s, `INSERT INTO bookings (business_id, event_id, category_id, created_by, status, expires_at,
		price_revision, rent, platform_fee, provider_fee, terms_snapshot, refund_policy_version, refund_policy_snapshot)
		VALUES ($1, $2, $3, $4, 'held', now() + interval '15 minutes', 1, 30000, 1000, 500, 't', 'v1', '{}') RETURNING id`,
		business, event, category, user)
	if _, err := s.DB.Exec(ctx, `INSERT INTO allocations (booking_id, event_id, event_slot_id, stand_id, status)
		VALUES ($1, $2, $3, $4, 'hold')`, id, event, slot, stand); err != nil {
		t.Fatal(err)
	}
	return id
}

func notify(attempt, status, code, gross string) (Notification, []byte) {
	n := Notification{OrderID: attempt, StatusCode: code, GrossAmount: gross, TransactionStatus: status,
		TransactionID: "tx-" + attempt, Currency: "IDR"}
	sum := sha512.Sum512([]byte(n.OrderID + n.StatusCode + n.GrossAmount + testKey))
	n.SignatureKey = hex.EncodeToString(sum[:])
	raw, _ := json.Marshal(n)
	return n, raw
}

func TestPaymentLifecycle(t *testing.T) {
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
	user := one(t, ctx, s, `INSERT INTO users (email, password_hash) VALUES ('p@example.com', 'x') RETURNING id`)
	other := one(t, ctx, s, `INSERT INTO users (email, password_hash) VALUES ('q@example.com', 'x') RETURNING id`)
	business := one(t, ctx, s, `INSERT INTO businesses (owner_id, category_id, name) VALUES ($1, $2, 'B') RETURNING id`, user, category)
	if _, err := s.DB.Exec(ctx, `INSERT INTO memberships (user_id, business_id, role) VALUES ($1, $2, 'owner')`, user, business); err != nil {
		t.Fatal(err)
	}
	org := one(t, ctx, s, `INSERT INTO organizations (owner_id, name) VALUES ($1, 'O') RETURNING id`, user)
	event := one(t, ctx, s, `INSERT INTO events (organization_id, created_by, name, slug, status, starts_at, ends_at,
		timezone, city, address, terms) VALUES ($1, $2, 'E', 'e', 'published', now(), now() + interval '2 hours',
		'Asia/Jakarta', 'Banda Aceh', 'St', 'T') RETURNING id`, org, user)
	if _, err := s.DB.Exec(ctx, `INSERT INTO event_layouts (event_id, revision, width, height, cell_meters, updated_by)
		VALUES ($1, 1, 8, 8, 1, $2)`, event, user); err != nil {
		t.Fatal(err)
	}
	slot := one(t, ctx, s, `INSERT INTO event_slots (event_id, code, starts_at, ends_at)
		SELECT id, 'whole', starts_at, ends_at FROM events WHERE id = $1 RETURNING id`, event)
	if _, err := s.DB.Exec(ctx, `INSERT INTO event_pricing_versions (event_id, revision, created_by, rules, effective_at)
		VALUES ($1, 1, $2, '{"default_base_price":30000}', now())`, event, user); err != nil {
		t.Fatal(err)
	}
	booking := seedBooking(t, ctx, s, user, business, category, event, slot, "00000000-0000-4000-8000-000000000001")

	// Another account cannot pay for this booking.
	if _, _, err := s.CreateAttempt(ctx, other, "q@example.com", booking, "k0", Input{Method: "snap"}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cross-account payment: %v", err)
	}
	attempt, created, err := s.CreateAttempt(ctx, user, "p@example.com", booking, "k1", Input{Method: "snap"})
	if err != nil || !created || attempt.Amount != 31500 || attempt.Status != "pending" || attempt.CheckoutURL == "" {
		t.Fatalf("attempt %+v created=%v err=%v", attempt, created, err)
	}
	if got := one(t, ctx, s, `SELECT status FROM bookings WHERE id = $1`, booking); got != "payment_pending" {
		t.Fatalf("booking status %s", got)
	}
	replay, created, err := s.CreateAttempt(ctx, user, "p@example.com", booking, "k1", Input{Method: "snap"})
	if err != nil || created || replay.ID != attempt.ID {
		t.Fatalf("replay %+v created=%v err=%v", replay, created, err)
	}
	if _, _, err := s.CreateAttempt(ctx, user, "p@example.com", booking, "k1", Input{Method: "snap", Description: "x"}); !errors.Is(err, ErrIdempotencyMismatch) {
		t.Fatalf("mismatch: %v", err)
	}
	again, created, err := s.CreateAttempt(ctx, user, "p@example.com", booking, "k2", Input{Method: "snap"})
	if err != nil || created || again.ID != attempt.ID {
		t.Fatalf("second key should reuse the live attempt: %+v %v", again, err)
	}

	// Wrong amount is rejected without touching the booking.
	n, raw := notify(attempt.ID, "settlement", "200", "1.00")
	if err := s.ApplyNotification(ctx, n, raw); err != nil {
		t.Fatal(err)
	}
	if got := one(t, ctx, s, `SELECT status FROM bookings WHERE id = $1`, booking); got != "payment_pending" {
		t.Fatalf("mismatch changed booking to %s", got)
	}
	if count(t, ctx, s, `SELECT count(*) FROM payment_exceptions WHERE kind = 'amount_mismatch'`) != 1 {
		t.Fatal("expected amount_mismatch exception")
	}

	// Settlement confirms once, even if delivered repeatedly or followed by a stale pending event.
	n, raw = notify(attempt.ID, "settlement", "200", "31500.00")
	for range 3 {
		if err := s.ApplyNotification(ctx, n, raw); err != nil {
			t.Fatal(err)
		}
	}
	stale, staleRaw := notify(attempt.ID, "pending", "201", "31500.00")
	if err := s.ApplyNotification(ctx, stale, staleRaw); err != nil {
		t.Fatal(err)
	}
	if got := one(t, ctx, s, `SELECT status FROM bookings WHERE id = $1`, booking); got != "confirmed" {
		t.Fatalf("booking %s", got)
	}
	if got := one(t, ctx, s, `SELECT status FROM allocations WHERE booking_id = $1`, booking); got != "reserved" {
		t.Fatalf("allocation %s", got)
	}
	if got := one(t, ctx, s, `SELECT status FROM payment_attempts WHERE id = $1`, attempt.ID); got != "paid" {
		t.Fatalf("attempt %s", got)
	}
	if count(t, ctx, s, `SELECT count(*) FROM ledger_transactions`) != 1 ||
		count(t, ctx, s, `SELECT count(*) FROM ledger_entries`) != 4 ||
		count(t, ctx, s, `SELECT count(*) FROM booking_status_history WHERE to_status = 'confirmed'`) != 1 {
		t.Fatal("expected exactly one confirmation and one ledger transaction")
	}
	if _, err := s.DB.Exec(ctx, `UPDATE ledger_entries SET amount = 1`); err == nil {
		t.Fatal("ledger must be append-only")
	}
	if _, err := s.DB.Exec(ctx, `INSERT INTO ledger_transactions (kind, ref_type, ref_id) VALUES ('reversal', 'x', gen_random_uuid())`); err != nil {
		t.Fatal(err)
	}

	// Payment arriving after the hold expired is an exception, never a silent confirmation.
	expired := seedBooking(t, ctx, s, user, business, category, event, slot, "00000000-0000-4000-8000-000000000002")
	late, _, err := s.CreateAttempt(ctx, user, "p@example.com", expired, "k3", Input{Method: "snap"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(ctx, `UPDATE bookings SET expires_at = now() - interval '1 minute' WHERE id = $1`, expired); err != nil {
		t.Fatal(err)
	}
	n, raw = notify(late.ID, "settlement", "200", "31500.00")
	expiryWorker := bookingstore.Store{DB: s.DB}
	expiry := make(chan error, 1)
	go func() {
		_, err := expiryWorker.Expire(ctx, 100)
		expiry <- err
	}()
	if err := s.ApplyNotification(ctx, n, raw); err != nil {
		t.Fatal(err)
	}
	if err := <-expiry; err != nil {
		t.Fatal(err)
	}
	if _, err := expiryWorker.Expire(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if got := one(t, ctx, s, `SELECT status FROM bookings WHERE id = $1`, expired); got != "expired" ||
		count(t, ctx, s, `SELECT count(*) FROM payment_exceptions WHERE kind = 'late_payment'`) != 1 ||
		one(t, ctx, s, `SELECT status FROM allocations WHERE booking_id = $1`, expired) != "released" {
		t.Fatalf("late payment: booking %s", got)
	}

	// A failed attempt returns the booking to held so the tenant can retry.
	retry := seedBooking(t, ctx, s, user, business, category, event, slot, "00000000-0000-4000-8000-000000000003")
	failing, _, err := s.CreateAttempt(ctx, user, "p@example.com", retry, "k4", Input{Method: "snap"})
	if err != nil {
		t.Fatal(err)
	}
	n, raw = notify(failing.ID, "expire", "407", "31500.00")
	if err := s.ApplyNotification(ctx, n, raw); err != nil {
		t.Fatal(err)
	}
	if got := one(t, ctx, s, `SELECT status FROM bookings WHERE id = $1`, retry); got != "held" {
		t.Fatalf("booking after failed attempt: %s", got)
	}
	next, created, err := s.CreateAttempt(ctx, user, "p@example.com", retry, "k5", Input{Method: "snap"})
	if err != nil || !created || next.ID == failing.ID {
		t.Fatalf("retry attempt %+v %v", next, err)
	}

	// The provider can receive a request even when its response is lost.
	timeout := seedBooking(t, ctx, s, user, business, category, event, slot, "00000000-0000-4000-8000-000000000004")
	var orders []string
	s.Provider.HTTP = &http.Client{Transport: snapTransport(func(req *http.Request) (*http.Response, error) {
		defer req.Body.Close()
		var input snapRequest
		if err := json.NewDecoder(req.Body).Decode(&input); err != nil {
			return nil, err
		}
		orders = append(orders, input.TransactionDetails.OrderID)
		if len(orders) == 1 {
			return nil, context.DeadlineExceeded
		}
		return &http.Response{StatusCode: http.StatusCreated,
			Body: io.NopCloser(strings.NewReader(`{"token":"recovered","redirect_url":"https://pay.example/recovered"}`))}, nil
	})}
	if _, _, err := s.CreateAttempt(ctx, user, "p@example.com", timeout, "k6", Input{Method: "snap"}); !errors.Is(err, ErrProvider) {
		t.Fatalf("expected provider timeout, got %v", err)
	}
	reserved := one(t, ctx, s, `SELECT id FROM payment_attempts WHERE booking_id = $1 AND status = 'created'`, timeout)
	recovered, created, err := s.CreateAttempt(ctx, user, "p@example.com", timeout, "k6", Input{Method: "snap"})
	if err != nil || created || recovered.ID != reserved || recovered.Status != "pending" || recovered.CheckoutURL == "" {
		t.Fatalf("timeout recovery %+v created=%v err=%v", recovered, created, err)
	}
	if len(orders) != 2 || orders[0] != reserved || orders[1] != reserved ||
		count(t, ctx, s, `SELECT count(*) FROM payment_attempts WHERE booking_id = $1`, timeout) != 1 {
		t.Fatalf("timeout created distinct payment attempts: %v", orders)
	}

	// Unknown orders are stored and flagged, not errors (so the provider stops retrying).
	n, raw = notify("not-a-uuid", "settlement", "200", "1.00")
	if err := s.ApplyNotification(ctx, n, raw); err != nil {
		t.Fatal(err)
	}
	if count(t, ctx, s, `SELECT count(*) FROM payment_exceptions WHERE kind = 'unknown_order'`) != 1 {
		t.Fatal("expected unknown_order exception")
	}
}

func TestEventPlanPurchase(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s := migratedStore(t, ctx)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"token":"tok","redirect_url":"https://pay.example/plan"}`))
	}))
	defer provider.Close()
	s.Provider = NewMidtrans(testKey, "sandbox")
	s.Provider.SnapURL = provider.URL

	owner := one(t, ctx, s, `INSERT INTO users (email, password_hash) VALUES ('owner@example.com', 'x') RETURNING id`)
	member := one(t, ctx, s, `INSERT INTO users (email, password_hash) VALUES ('member@example.com', 'x') RETURNING id`)
	stranger := one(t, ctx, s, `INSERT INTO users (email, password_hash) VALUES ('stranger@example.com', 'x') RETURNING id`)
	org := one(t, ctx, s, `INSERT INTO organizations (owner_id, name) VALUES ($1, 'Org') RETURNING id`, owner)
	for user, role := range map[string]string{owner: "owner", member: "member"} {
		if _, err := s.DB.Exec(ctx, `INSERT INTO memberships (user_id, organization_id, role) VALUES ($1, $2, $3)`, user, org, role); err != nil {
			t.Fatal(err)
		}
	}
	newEvent := func(slug string) string {
		return one(t, ctx, s, `INSERT INTO events (organization_id, created_by, name, slug, status, starts_at, ends_at,
			timezone, city, address, terms) VALUES ($1, $2, 'Bazar', $3, 'draft', now(), now() + interval '2 days',
			'Asia/Jakarta', 'Banda Aceh', 'St', 'T') RETURNING id`, org, owner, slug)
	}

	custom := newEvent("custom")
	capacity := 120
	customInput := PlanInput{PlanCode: "custom", Method: "snap", CapacityStands: &capacity}
	customCheckout, _, err := s.PurchasePlan(ctx, owner, "owner@example.com", custom, "custom-1", customInput)
	if err != nil || customCheckout.Purchase.CapacityStands == nil || *customCheckout.Purchase.CapacityStands != capacity ||
		customCheckout.Purchase.Amount != 700000 || customCheckout.Payment == nil || customCheckout.Payment.Amount != 700000 {
		t.Fatalf("custom price or capacity: %+v, %v", customCheckout, err)
	}
	capacity++
	if _, _, err := s.PurchasePlan(ctx, owner, "owner@example.com", custom, "custom-1", customInput); !errors.Is(err, ErrIdempotencyMismatch) {
		t.Fatalf("custom capacity must be part of the idempotency key: %v", err)
	}

	// A free plan activates at once and creates no payment.
	if _, err := s.DB.Exec(ctx, `UPDATE event_plans SET active = true WHERE code = 'standard'`); err != nil {
		t.Fatal(err)
	}
	free := newEvent("free")
	got, created, err := s.PurchasePlan(ctx, owner, "owner@example.com", free, "free-1", PlanInput{PlanCode: "standard"})
	if err != nil || !created || got.Purchase.Status != "active" || got.Payment != nil {
		t.Fatalf("free plan: %+v created=%v err=%v", got, created, err)
	}
	again, created, err := s.PurchasePlan(ctx, owner, "owner@example.com", free, "free-2", PlanInput{PlanCode: "standard"})
	if err != nil || created || again.Purchase.ID != got.Purchase.ID {
		t.Fatalf("second purchase should report the active plan: %+v created=%v err=%v", again, created, err)
	}
	if count(t, ctx, s, `SELECT count(*) FROM payment_attempts WHERE purchase_id = $1`, got.Purchase.ID) != 0 {
		t.Fatal("free plan must not open a payment")
	}
	if state, err := s.PlanStatus(ctx, member, free); err != nil || !state.Active {
		t.Fatalf("members can read plan status: %+v %v", state, err)
	}
	if _, err := s.PlanStatus(ctx, stranger, free); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stranger read plan status: %v", err)
	}

	// Paid plan: only owners buy; price comes from the plan, never from the client.
	if _, err := s.DB.Exec(ctx, `UPDATE event_plans SET price = 100000 WHERE code = 'standard'`); err != nil {
		t.Fatal(err)
	}
	event := newEvent("paid")
	if _, _, err := s.PurchasePlan(ctx, member, "member@example.com", event, "m-1", PlanInput{PlanCode: "standard", Method: "snap"}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("member purchase: %v", err)
	}
	if _, _, err := s.PurchasePlan(ctx, owner, "owner@example.com", event, "u-1", PlanInput{PlanCode: "ghost", Method: "snap"}); !errors.Is(err, ErrPlanUnknown) {
		t.Fatalf("unknown plan: %v", err)
	}
	checkout, created, err := s.PurchasePlan(ctx, owner, "owner@example.com", event, "p-1", PlanInput{PlanCode: "standard", Method: "snap"})
	if err != nil || !created || checkout.Purchase.Status != "pending" || checkout.Payment == nil ||
		checkout.Payment.Amount != 100000 || checkout.Payment.CheckoutURL == "" || checkout.Payment.Status != "pending" {
		t.Fatalf("paid plan: %+v created=%v err=%v", checkout, created, err)
	}
	replay, created, err := s.PurchasePlan(ctx, owner, "owner@example.com", event, "p-1", PlanInput{PlanCode: "standard", Method: "snap"})
	if err != nil || created || replay.Payment == nil || replay.Payment.ID != checkout.Payment.ID {
		t.Fatalf("replay: %+v created=%v err=%v", replay, created, err)
	}
	if _, _, err := s.PurchasePlan(ctx, owner, "owner@example.com", free, "p-1", PlanInput{PlanCode: "standard", Method: "snap"}); !errors.Is(err, ErrIdempotencyMismatch) {
		t.Fatalf("key reused for another event: %v", err)
	}
	reuse, created, err := s.PurchasePlan(ctx, owner, "owner@example.com", event, "p-2", PlanInput{PlanCode: "standard", Method: "snap"})
	if err != nil || created || reuse.Payment == nil || reuse.Payment.ID != checkout.Payment.ID {
		t.Fatalf("a second key must reuse the live attempt: %+v created=%v err=%v", reuse, created, err)
	}

	// A wrong amount cannot activate the plan.
	n, raw := notify(checkout.Payment.ID, "settlement", "200", "1.00")
	if err := s.ApplyNotification(ctx, n, raw); err != nil {
		t.Fatal(err)
	}
	if got := one(t, ctx, s, `SELECT status FROM event_plan_purchases WHERE id = $1`, checkout.Purchase.ID); got != "pending" {
		t.Fatalf("mismatch activated the plan: %s", got)
	}

	// Settlement activates once, books platform revenue, and ignores replays and stale statuses.
	n, raw = notify(checkout.Payment.ID, "settlement", "200", "100000.00")
	for range 3 {
		if err := s.ApplyNotification(ctx, n, raw); err != nil {
			t.Fatal(err)
		}
	}
	stale, staleRaw := notify(checkout.Payment.ID, "pending", "201", "100000.00")
	if err := s.ApplyNotification(ctx, stale, staleRaw); err != nil {
		t.Fatal(err)
	}
	if got := one(t, ctx, s, `SELECT status FROM event_plan_purchases WHERE id = $1`, checkout.Purchase.ID); got != "active" {
		t.Fatalf("purchase %s", got)
	}
	if count(t, ctx, s, `SELECT count(*) FROM ledger_transactions WHERE kind = 'plan_payment_received'`) != 1 ||
		count(t, ctx, s, `SELECT count(*) FROM ledger_entries e JOIN ledger_transactions t ON t.id = e.transaction_id
			WHERE t.kind = 'plan_payment_received' AND e.account = 'platform_revenue' AND e.direction = 'credit' AND e.amount = 100000`) != 1 {
		t.Fatal("expected one balanced plan ledger transaction crediting platform revenue")
	}
	if count(t, ctx, s, `SELECT count(*) FROM ledger_transactions WHERE kind = 'payment_received'`) != 0 {
		t.Fatal("plan payment must not be booked as rent escrow")
	}
	if state, err := s.PlanStatus(ctx, owner, event); err != nil || !state.Active || state.Payment != nil {
		t.Fatalf("status after payment: %+v %v", state, err)
	}

	// An expired payment leaves the purchase open so the organizer can try again.
	retry := newEvent("retry")
	first, _, err := s.PurchasePlan(ctx, owner, "owner@example.com", retry, "r-1", PlanInput{PlanCode: "standard", Method: "snap"})
	if err != nil {
		t.Fatal(err)
	}
	n, raw = notify(first.Payment.ID, "expire", "407", "100000.00")
	if err := s.ApplyNotification(ctx, n, raw); err != nil {
		t.Fatal(err)
	}
	if got := one(t, ctx, s, `SELECT status FROM payment_attempts WHERE id = $1`, first.Payment.ID); got != "expired" {
		t.Fatalf("attempt %s", got)
	}
	second, created, err := s.PurchasePlan(ctx, owner, "owner@example.com", retry, "r-2", PlanInput{PlanCode: "standard", Method: "snap"})
	if err != nil || !created || second.Payment == nil || second.Payment.ID == first.Payment.ID || second.Purchase.ID != first.Purchase.ID {
		t.Fatalf("retry: %+v created=%v err=%v", second, created, err)
	}
	// Replaying the old key still reports the old, expired attempt rather than opening another one.
	old, created, err := s.PurchasePlan(ctx, owner, "owner@example.com", retry, "r-1", PlanInput{PlanCode: "standard", Method: "snap"})
	if err != nil || created || old.Payment == nil || old.Payment.ID != first.Payment.ID {
		t.Fatalf("old key replay: %+v created=%v err=%v", old, created, err)
	}

	// A cancelled event cannot buy a plan.
	cancelled := newEvent("cancelled")
	if _, err := s.DB.Exec(ctx, `UPDATE events SET status = 'cancelled' WHERE id = $1`, cancelled); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.PurchasePlan(ctx, owner, "owner@example.com", cancelled, "c-1", PlanInput{PlanCode: "standard", Method: "snap"}); !errors.Is(err, ErrPlanState) {
		t.Fatalf("cancelled event: %v", err)
	}
}
