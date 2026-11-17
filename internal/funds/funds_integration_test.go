package funds

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"kitakarsa.com/tenantku/backend/internal/payment"
)

const testKey = "Mid-server-test"

func migrated(t *testing.T, ctx context.Context) Store {
	t.Helper()
	databaseURL := os.Getenv("TENANTKU_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set TENANTKU_TEST_DATABASE_URL to run the PostgreSQL funds test")
	}
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	schema := pgx.Identifier{"funds_test_" + strings.ToLower(rand.Text())}.Sanitize()
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
		// This deployment function is bound to public, while the fixture uses its own schema.
		if !strings.HasSuffix(f.Name(), ".up.sql") || f.Name() == "00013_cancel_unpaid_event_bookings.up.sql" {
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

func num(t *testing.T, ctx context.Context, s Store, query string, args ...any) int64 {
	t.Helper()
	var v int64
	if err := s.DB.QueryRow(ctx, query, args...).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func exec(t *testing.T, ctx context.Context, s Store, query string, args ...any) {
	t.Helper()
	if _, err := s.DB.Exec(ctx, query, args...); err != nil {
		t.Fatal(err)
	}
}

type world struct {
	owner, staff, tenant, admin, stranger string
	org, business, category, event, slot  string
	standSeq                              int
}

// newWorld makes an organization (owner and staff), a published event, and a tenant business. The event ends
// endsAgo in the past so payout timing can be tested around the H+4 boundary.
func newWorld(t *testing.T, ctx context.Context, s Store, endsAt time.Time) *world {
	t.Helper()
	w := &world{category: one(t, ctx, s, `SELECT id FROM business_categories ORDER BY id LIMIT 1`)}
	user := func(email string) string {
		return one(t, ctx, s, `INSERT INTO users (email, password_hash) VALUES ($1, 'x') RETURNING id`, email)
	}
	w.owner, w.staff, w.tenant, w.admin, w.stranger = user("o@e.com"), user("s@e.com"), user("t@e.com"), user("a@e.com"), user("x@e.com")
	exec(t, ctx, s, `INSERT INTO platform_admins (user_id) VALUES ($1)`, w.admin)
	w.org = one(t, ctx, s, `INSERT INTO organizations (owner_id, name) VALUES ($1, 'EO') RETURNING id`, w.owner)
	exec(t, ctx, s, `INSERT INTO memberships (user_id, organization_id, role) VALUES ($1, $3, 'owner'), ($2, $3, 'member')`, w.owner, w.staff, w.org)
	w.business = one(t, ctx, s, `INSERT INTO businesses (owner_id, category_id, name) VALUES ($1, $2, 'Kopi Senja') RETURNING id`, w.tenant, w.category)
	exec(t, ctx, s, `INSERT INTO memberships (user_id, business_id, role) VALUES ($1, $2, 'owner')`, w.tenant, w.business)
	w.event = one(t, ctx, s, `INSERT INTO events (organization_id, created_by, name, slug, status, starts_at, ends_at, timezone, city, address, terms)
		VALUES ($1, $2, 'Pasar Malam', 'pasar', 'published', $3, $4, 'Asia/Jakarta', 'Banda Aceh', 'St', 'T') RETURNING id`,
		w.org, w.owner, endsAt.Add(-3*time.Hour), endsAt)
	exec(t, ctx, s, `INSERT INTO event_layouts (event_id, revision, width, height, cell_meters, updated_by) VALUES ($1, 1, 20, 20, 1, $2)`, w.event, w.owner)
	w.slot = one(t, ctx, s, `INSERT INTO event_slots (event_id, code, starts_at, ends_at) SELECT id, 'whole', starts_at, ends_at FROM events WHERE id = $1 RETURNING id`, w.event)
	exec(t, ctx, s, `INSERT INTO event_pricing_versions (event_id, revision, created_by, rules, effective_at) VALUES ($1, 1, $2, '{"default_base_price":100000}', now())`, w.event, w.owner)
	return w
}

// booking adds a booking in the given state with a stand allocation.
func (w *world) booking(t *testing.T, ctx context.Context, s Store, status, allocation string, rent int64) string {
	t.Helper()
	w.standSeq++
	stand := fmt.Sprintf("00000000-0000-4000-8000-%012d", w.standSeq)
	exec(t, ctx, s, `INSERT INTO stands (id, event_id, code, x, y, width, height) VALUES ($1::uuid, $2, $3, $4, $5, 1, 1)`,
		stand, w.event, fmt.Sprintf("S%d", w.standSeq), w.standSeq%20, w.standSeq/20%20)
	id := one(t, ctx, s, `INSERT INTO bookings (business_id, event_id, category_id, created_by, status, expires_at, price_revision, rent, platform_fee, provider_fee,
		terms_snapshot, refund_policy_version, refund_policy_snapshot) VALUES ($1, $2, $3, $4, $5, now() + interval '1 hour', 1, $6, 0, 0, 't', 'v1', '{}') RETURNING id`,
		w.business, w.event, w.category, w.tenant, status, rent)
	exec(t, ctx, s, `INSERT INTO allocations (booking_id, event_id, event_slot_id, stand_id, status) VALUES ($1, $2, $3, $4, $5)`, id, w.event, w.slot, stand, allocation)
	exec(t, ctx, s, `INSERT INTO booking_status_history (booking_id, from_status, to_status, reason) VALUES ($1, NULL, 'held', NULL)`, id)
	return id
}

// paid makes a confirmed booking whose payment was received: rent plus admin fee in, split in the ledger.
func (w *world) paid(t *testing.T, ctx context.Context, s Store, method string, rent, fee int64) (booking, attempt string) {
	t.Helper()
	booking = w.booking(t, ctx, s, "confirmed", "reserved", rent)
	exec(t, ctx, s, `INSERT INTO booking_status_history (booking_id, from_status, to_status) VALUES ($1, 'held', 'confirmed')`, booking)
	attempt = one(t, ctx, s, `INSERT INTO payment_attempts (booking_id, created_by, provider, method, status, amount, expires_at, idempotency_key, request_digest,
		paid_at, rent_amount, platform_fee_amount, provider_fee_amount) VALUES ($1, $2, 'midtrans', $3, 'paid', $4, now() + interval '1 hour', gen_random_uuid()::text,
		decode(repeat('00', 32), 'hex'), now(), $5, 0, $6) RETURNING id`, booking, w.tenant, method, rent+fee, rent, fee)
	tx := one(t, ctx, s, `INSERT INTO ledger_transactions (kind, ref_type, ref_id) VALUES ('payment_received', 'payment_attempt', $1) RETURNING id`, attempt)
	exec(t, ctx, s, `INSERT INTO ledger_entries (transaction_id, account, direction, amount) VALUES
		($1, 'provider_clearing', 'debit', $2), ($1, 'organizer_escrow', 'credit', $3), ($1, 'provider_fee_payable', 'credit', $4)`, tx, rent+fee, rent, fee)
	return booking, attempt
}

func (s Store) escrow(t *testing.T, ctx context.Context) map[string]int64 {
	t.Helper()
	rows, err := s.DB.Query(ctx, `SELECT account, SUM(CASE direction WHEN 'debit' THEN amount ELSE -amount END)::bigint FROM ledger_entries GROUP BY account`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var a string
		var n int64
		if err := rows.Scan(&a, &n); err != nil {
			t.Fatal(err)
		}
		out[a] = n
	}
	var total int64
	for _, n := range out {
		total += n
	}
	if total != 0 {
		t.Fatalf("ledger does not balance: %v", out)
	}
	return out
}

func cancel(t *testing.T, ctx context.Context, s Store, w *world) (Settlement, error) {
	t.Helper()
	var out Settlement
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE events SET status = 'cancelled' WHERE id = $1`, w.event); err != nil {
			return err
		}
		var err error
		out, err = SettleCancellation(ctx, tx, w.event, w.owner)
		return err
	})
	return out, err
}

type fakeProvider struct {
	mu      sync.Mutex
	refund  string
	status  string
	refunds []string
	expired []string
	calls   int
}

func (f *fakeProvider) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.HasSuffix(r.URL.Path, "/refund"):
		f.calls++
		f.refunds = append(f.refunds, strings.Split(r.URL.Path, "/")[1])
		_, _ = w.Write([]byte(f.refund))
	case strings.HasSuffix(r.URL.Path, "/expire"):
		f.expired = append(f.expired, strings.Split(r.URL.Path, "/")[1])
		_, _ = w.Write([]byte(`{"status_code":"404","status_message":"Transaction doesn't exist."}`))
	case strings.HasSuffix(r.URL.Path, "/status"):
		_, _ = w.Write([]byte(f.status))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// T08: the organizer cancels. With rent Rp100.000 and an admin fee of Rp2.000 the tenant gets Rp50.000 back.
func TestCancellationRefundsHalfTheRent(t *testing.T) {
	ctx, cancelCtx := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelCtx()
	s := migrated(t, ctx)
	fake := &fakeProvider{refund: `{"status_code":"200","refund_key":"rk-1"}`}
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer server.Close()
	s.Provider = payment.NewMidtrans(testKey, "sandbox")
	s.Provider.StatusURL = server.URL
	w := newWorld(t, ctx, s, time.Now().Add(48*time.Hour))
	exec(t, ctx, s, `UPDATE payment_methods SET enabled = true WHERE code = 'gopay'`)

	api, apiAttempt := w.paid(t, ctx, s, "gopay", 100000, 2000) // refundable through the provider API
	manual, _ := w.paid(t, ctx, s, "bca_va", 100000, 2000)      // a virtual account has to be paid back by hand
	hold := w.booking(t, ctx, s, "held", "hold", 100000)        // not paid: simply released
	offer := w.booking(t, ctx, s, "held", "offer", 100000)      // a curated offer: also released
	pending := w.booking(t, ctx, s, "payment_pending", "hold", 100000)
	exec(t, ctx, s, `INSERT INTO payment_attempts (booking_id, created_by, provider, method, status, amount, expires_at, idempotency_key, request_digest,
		rent_amount, platform_fee_amount, provider_fee_amount) VALUES ($1, $2, 'midtrans', 'bca_va', 'pending', 104000, now() + interval '1 hour', 'p1',
		decode(repeat('00', 32), 'hex'), 100000, 0, 4000)`, pending, w.tenant)
	exec(t, ctx, s, `INSERT INTO applications (event_id, business_id, submitted_by) VALUES ($1, $2, $3)`, w.event, w.business, w.tenant)

	before := s.escrow(t, ctx)
	settled, err := cancel(t, ctx, s, w)
	if err != nil {
		t.Fatal(err)
	}
	if settled.ClosedHolds != 2 || settled.ClosedPending != 1 || settled.Refunds != 2 || settled.RefundTotal != 100000 {
		t.Fatalf("settlement %+v", settled)
	}
	for _, id := range []string{api, manual, hold, offer, pending} {
		if one(t, ctx, s, `SELECT status FROM bookings WHERE id = $1`, id) != "cancelled" ||
			one(t, ctx, s, `SELECT status FROM allocations WHERE booking_id = $1`, id) != "released" ||
			num(t, ctx, s, `SELECT count(*) FROM booking_status_history WHERE booking_id = $1 AND to_status = 'cancelled' AND reason = 'event_cancelled'`, id) != 1 {
			t.Fatalf("booking %s not closed and released", id)
		}
	}
	// Exactly Rp50.000 of Rp100.000 rent, whatever the admin fee was.
	if num(t, ctx, s, `SELECT amount FROM refunds WHERE booking_id = $1`, api) != 50000 || num(t, ctx, s, `SELECT rent_amount FROM refunds WHERE booking_id = $1`, api) != 100000 {
		t.Fatal("refund must be 50% of the rent snapshot")
	}
	// The unpaid payment is closed locally and queued to be closed at the provider; no refund exists for it.
	if one(t, ctx, s, `SELECT status FROM payment_attempts WHERE booking_id = $1`, pending) != "expired" ||
		num(t, ctx, s, `SELECT count(*) FROM payment_closures WHERE status = 'pending'`) != 1 ||
		num(t, ctx, s, `SELECT count(*) FROM refunds WHERE booking_id IN ($1, $2, $3)`, hold, offer, pending) != 0 {
		t.Fatal("pending payment was not closed correctly")
	}
	if one(t, ctx, s, `SELECT status FROM applications LIMIT 1`) != "withdrawn" {
		t.Fatal("waiting application was not withdrawn")
	}
	// The refund is reserved out of escrow now; the admin fee is untouched.
	after := s.escrow(t, ctx)
	if after["organizer_escrow"]-before["organizer_escrow"] != 100000 || after["refund_payable"] != -100000 || after["provider_fee_payable"] != -4000 {
		t.Fatalf("ledger after cancellation: before %v after %v", before, after)
	}
	// Cancelling again changes nothing.
	again, err := cancel(t, ctx, s, w)
	if err != nil || again.Refunds != 0 || num(t, ctx, s, `SELECT count(*) FROM refunds`) != 2 || s.escrow(t, ctx)["refund_payable"] != -100000 {
		t.Fatalf("second settlement %+v %v", again, err)
	}

	// Processing: the provider refunds the gopay payment once; the virtual account waits for a person.
	report, err := s.Process(ctx)
	if err != nil || report.Refunded != 1 || report.ManualRequired != 1 || report.ClosedTransactions != 1 {
		t.Fatalf("process %+v %v", report, err)
	}
	if len(fake.refunds) != 1 || fake.refunds[0] != apiAttempt {
		t.Fatalf("provider refund calls %v", fake.refunds)
	}
	if one(t, ctx, s, `SELECT status FROM refunds WHERE booking_id = $1`, api) != "refunded" ||
		one(t, ctx, s, `SELECT status FROM refunds WHERE booking_id = $1`, manual) != "manual_required" ||
		num(t, ctx, s, `SELECT count(*) FROM payment_exceptions WHERE kind = 'manual_refund_required' AND status = 'open'`) != 1 {
		t.Fatal("refund states after processing")
	}
	if _, err := s.Process(ctx); err != nil || fake.calls != 1 {
		t.Fatalf("a second pass sent the refund again: %v calls=%d", err, fake.calls)
	}
	mid := s.escrow(t, ctx)
	if mid["refund_payable"] != -50000 || mid["provider_clearing"] != 2*102000-50000 {
		t.Fatalf("ledger after the provider refund %v", mid)
	}

	// The operator confirms the manual refund. Only an admin can, and only once.
	manualRefund := one(t, ctx, s, `SELECT id FROM refunds WHERE booking_id = $1`, manual)
	if _, err := s.ConfirmRefund(ctx, w.owner, manualRefund, "BCA-1", 0); !errors.Is(err, ErrAdminRequired) {
		t.Fatalf("non-admin confirmed a refund: %v", err)
	}
	if _, err := s.ConfirmRefund(ctx, w.admin, manualRefund, "BCA-1", 1500); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfirmRefund(ctx, w.admin, manualRefund, "BCA-1", 1500); err != nil {
		t.Fatalf("a repeat confirmation must be a no-op: %v", err)
	}
	final := s.escrow(t, ctx)
	// The Rp1.500 processing fee is charged to the organizer's escrow, not the tenant's refund.
	if final["refund_payable"] != 0 || final["organizer_escrow"]-before["organizer_escrow"] != 100000+1500 ||
		final["provider_fee_payable"] != -4000-1500 || num(t, ctx, s, `SELECT count(*) FROM payment_exceptions WHERE status = 'open' AND kind = 'manual_refund_required'`) != 0 {
		t.Fatalf("final ledger %v", final)
	}
	if num(t, ctx, s, `SELECT count(*) FROM admin_audit WHERE action = 'refund.confirm'`) != 2 {
		t.Fatal("admin actions must be audited")
	}

	// A provider that refuses the refund sends it to a person instead of losing it.
	w2 := newWorld2(t, ctx, s, w)
	_, attempt2 := w2.paid(t, ctx, s, "gopay", 100000, 2000)
	fake.refund = `{"status_code":"412","status_message":"Merchant cannot modify status of the transaction."}`
	fake.status = `{"order_id":"x","transaction_status":"settlement"}`
	if _, err := cancel(t, ctx, s, w2); err != nil {
		t.Fatal(err)
	}
	report, err = s.Process(ctx)
	if err != nil || report.ManualRequired != 1 || report.Refunded != 0 || fake.refunds[len(fake.refunds)-1] != attempt2 {
		t.Fatalf("unsupported refund %+v %v", report, err)
	}
	// A refund the provider already made (the retry after a lost reply) is recognised, not asked for again.
	w3 := newWorld2(t, ctx, s, w)
	_, attempt3 := w3.paid(t, ctx, s, "gopay", 100000, 2000)
	fake.status = `{"order_id":"` + attempt3 + `","status_code":"200","transaction_status":"partial_refund"}`
	if _, err := cancel(t, ctx, s, w3); err != nil {
		t.Fatal(err)
	}
	report, err = s.Process(ctx)
	if err != nil || report.Refunded != 1 {
		t.Fatalf("duplicate refund %+v %v", report, err)
	}
}

// nextBlock gives each extra event its own range of stand numbers, since stand IDs are derived from them.
var nextBlock = 1000

// newWorld2 adds another event under the same people, for scenarios that need a fresh event.
func newWorld2(t *testing.T, ctx context.Context, s Store, base *world) *world {
	t.Helper()
	w := *base
	nextBlock += 100
	w.standSeq = nextBlock
	slug := strings.ToLower(rand.Text())[:8]
	w.event = one(t, ctx, s, `INSERT INTO events (organization_id, created_by, name, slug, status, starts_at, ends_at, timezone, city, address, terms)
		VALUES ($1, $2, 'Event Lain', $3, 'published', now() + interval '1 day', now() + interval '2 days', 'Asia/Jakarta', 'Banda Aceh', 'St', 'T') RETURNING id`, w.org, w.owner, slug)
	exec(t, ctx, s, `INSERT INTO event_layouts (event_id, revision, width, height, cell_meters, updated_by) VALUES ($1, 1, 20, 20, 1, $2)`, w.event, w.owner)
	w.slot = one(t, ctx, s, `INSERT INTO event_slots (event_id, code, starts_at, ends_at) SELECT id, 'whole', starts_at, ends_at FROM events WHERE id = $1 RETURNING id`, w.event)
	exec(t, ctx, s, `INSERT INTO event_pricing_versions (event_id, revision, created_by, rules, effective_at) VALUES ($1, 1, $2, '{"default_base_price":100000}', now())`, w.event, w.owner)
	return &w
}

func TestWorkingDays(t *testing.T) {
	day := func(v string) time.Time {
		d, err := time.Parse("2006-01-02", v)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	for name, tc := range map[string]struct {
		last string
		off  map[string]bool
		want string
	}{
		"friday skips the weekend": {"2026-11-13", nil, "2026-11-19"},
		"monday":                   {"2026-11-16", nil, "2026-11-20"},
		"saturday starts monday":   {"2026-11-14", nil, "2026-11-19"},
		"sunday starts monday":     {"2026-11-15", nil, "2026-11-19"},
		"a holiday pushes it back": {"2026-11-16", map[string]bool{"2026-11-18": true}, "2026-11-23"},
		"holiday on the weekend":   {"2026-11-13", map[string]bool{"2026-11-14": true}, "2026-11-19"},
	} {
		if got := AddWorkingDays(day(tc.last), PayoutWorkingDays, tc.off).Format("2006-01-02"); got != tc.want {
			t.Errorf("%s: %s + 4 working days = %s, want %s", name, tc.last, got, tc.want)
		}
	}
}

// T09: payout from H+4 working days, once, and not while a refund or dispute is open.
func TestPayoutRules(t *testing.T) {
	ctx, cancelCtx := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelCtx()
	s := migrated(t, ctx)
	jakarta, _ := time.LoadLocation("Asia/Jakarta")
	today := time.Now().In(jakarta)
	// Find the last day whose H+4 is today, so the boundary is tested exactly.
	last := today.AddDate(0, 0, -4)
	for AddWorkingDays(last, PayoutWorkingDays, nil).Format(dateLayout) != today.Format(dateLayout) {
		last = last.AddDate(0, 0, -1)
	}
	endsToday := time.Date(last.Year(), last.Month(), last.Day(), 18, 0, 0, 0, jakarta) // H+4 is today
	w := newWorld(t, ctx, s, endsToday)
	booking, _ := w.paid(t, ctx, s, "bca_va", 100000, 2000)
	state := func() PayoutState {
		st, err := s.PayoutStateFor(ctx, w.owner, w.event)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	has := func(st PayoutState, code string) bool {
		for _, b := range st.Blockers {
			if b == code {
				return true
			}
		}
		return false
	}

	st := state()
	if st.AvailableFrom != today.Format(dateLayout) || has(st, BlockedNotEnded) || st.Available != 100000 || !has(st, BlockedNoBankAccount) {
		t.Fatalf("on the H+4 day: %+v", st)
	}
	// One day too early: an event whose H+4 is tomorrow is still blocked, and a request is refused.
	tomorrow := today.AddDate(0, 0, 1)
	late := today
	for AddWorkingDays(late, PayoutWorkingDays, nil).Format(dateLayout) != tomorrow.Format(dateLayout) {
		late = late.AddDate(0, 0, -1)
	}
	endsLate := time.Date(late.Year(), late.Month(), late.Day(), 18, 0, 0, 0, jakarta)
	exec(t, ctx, s, `UPDATE events SET ends_at = $2::timestamptz, starts_at = $2::timestamptz - interval '3 hours' WHERE id = $1`, w.event, endsLate)
	if early := state(); !has(early, BlockedNotEnded) || early.AvailableFrom != tomorrow.Format(dateLayout) || early.Eligible {
		t.Fatalf("the day before H+4 must be blocked: %+v", early)
	}
	if _, _, err := s.RequestPayout(ctx, w.owner, w.event, "early"); !errors.Is(err, ErrPayoutBlocked) {
		t.Fatalf("early payout request: %v", err)
	}
	exec(t, ctx, s, `UPDATE events SET ends_at = $2::timestamptz, starts_at = $2::timestamptz - interval '3 hours' WHERE id = $1`, w.event, endsToday)

	// Access: only the owner may request, and only with a bank account.
	if _, _, err := s.RequestPayout(ctx, w.staff, w.event, "k"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("staff requested a payout: %v", err)
	}
	var blocked BlockedError
	if _, _, err := s.RequestPayout(ctx, w.owner, w.event, "k"); !errors.As(err, &blocked) || blocked.Blockers[0] != BlockedNoBankAccount {
		t.Fatalf("without a bank account: %v", err)
	}
	if _, err := s.SetBankAccount(ctx, w.staff, w.org, BankAccount{BankCode: "bca", AccountNumber: "1234567890", HolderName: "EO"}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("staff changed the bank account: %v", err)
	}
	if _, err := s.SetBankAccount(ctx, w.owner, w.org, BankAccount{BankCode: "BCA", AccountNumber: "12 34-567890", HolderName: " EO   Nusantara "}); err != nil {
		t.Fatal(err)
	}
	if shown, _ := s.GetBankAccount(ctx, w.staff, w.org); shown == nil || shown.AccountNumber != "••••••7890" || shown.HolderName != "EO Nusantara" {
		t.Fatalf("masked account %+v", shown)
	}
	if _, err := s.SetBankAccount(ctx, w.owner, w.org, BankAccount{BankCode: "bca", AccountNumber: "12ab", HolderName: "EO"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid account number: %v", err)
	}

	// A dispute blocks the payout until an admin resolves it.
	dispute, err := s.OpenDispute(ctx, w.tenant, booking, "Stand tidak sesuai")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenDispute(ctx, w.tenant, booking, "lagi"); !errors.Is(err, ErrDisputeOpen) {
		t.Fatalf("second open dispute: %v", err)
	}
	if _, err := s.OpenDispute(ctx, w.stranger, booking, "bukan saya"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("another account opened a dispute: %v", err)
	}
	if st := state(); !has(st, BlockedOpenDispute) || st.Eligible {
		t.Fatalf("open dispute must block: %+v", st)
	}
	// A refund larger than what remains of the rent is refused; a smaller one is owed and blocks until paid.
	if _, _, err := s.ResolveDispute(ctx, w.admin, dispute.ID, "refund", 100001, "x"); !errors.Is(err, ErrAmountTooHigh) {
		t.Fatalf("refund above the rent: %v", err)
	}
	if _, _, err := s.ResolveDispute(ctx, w.owner, dispute.ID, "reject", 0, "x"); !errors.Is(err, ErrAdminRequired) {
		t.Fatalf("non-admin resolved a dispute: %v", err)
	}
	resolved, refund, err := s.ResolveDispute(ctx, w.admin, dispute.ID, "refund", 20000, "Sebagian stand tidak tersedia")
	if err != nil || resolved.Status != "refunded" || refund == nil || refund.Amount != 20000 || refund.Reason != "dispute" {
		t.Fatalf("resolve %+v %+v %v", resolved, refund, err)
	}
	if _, _, err := s.ResolveDispute(ctx, w.admin, dispute.ID, "reject", 0, "x"); !errors.Is(err, ErrState) {
		t.Fatalf("resolving twice: %v", err)
	}
	if st := state(); !has(st, BlockedOpenRefund) || has(st, BlockedOpenDispute) || st.Available != 80000 {
		t.Fatalf("open refund must block and be reserved: %+v", st)
	}
	if _, err := s.ConfirmRefund(ctx, w.admin, refund.ID, "BCA-2", 0); err != nil {
		t.Fatal(err)
	}
	st = state()
	if !st.Eligible || st.Available != 80000 {
		t.Fatalf("after the refund is paid: %+v", st)
	}

	// The request sets the amount aside, is idempotent, and cannot be made twice for the event.
	payout, created, err := s.RequestPayout(ctx, w.owner, w.event, "k1")
	if err != nil || !created || payout.Amount != 80000 || payout.Status != "requested" || payout.Account != "••••••7890" {
		t.Fatalf("payout %+v %v %v", payout, created, err)
	}
	same, created, err := s.RequestPayout(ctx, w.owner, w.event, "k1")
	if err != nil || created || same.ID != payout.ID {
		t.Fatalf("same key %+v %v %v", same, created, err)
	}
	if _, _, err := s.RequestPayout(ctx, w.owner, w.event, "k2"); !errors.As(err, &blocked) || !strings.Contains(strings.Join(blocked.Blockers, ","), BlockedAlreadyPaid) {
		t.Fatalf("a second request with a new key: %v", err)
	}
	if got := num(t, ctx, s, `SELECT count(*) FROM payouts`); got != 1 {
		t.Fatalf("%d payouts exist", got)
	}
	if _, err := s.SetBankAccount(ctx, w.owner, w.org, BankAccount{BankCode: "bni", AccountNumber: "9999999", HolderName: "Lain"}); !errors.Is(err, ErrState) {
		t.Fatalf("the account changed under a pending payout: %v", err)
	}
	// A dispute can still be raised before release, and it freezes nothing already reserved but is visible to the admin.
	// Confirmation is the operator's, and the ledger moves only then.
	if _, err := s.ReleasePayout(ctx, w.owner, payout.ID, "TRF-1"); !errors.Is(err, ErrAdminRequired) {
		t.Fatalf("owner released their own payout: %v", err)
	}
	if mid := s.escrow(t, ctx); mid["payout_payable"] != -80000 || mid["organizer_escrow"] != -100000+20000+80000 {
		t.Fatalf("reserved ledger %v", mid)
	}
	released, err := s.ReleasePayout(ctx, w.admin, payout.ID, "TRF-1")
	if err != nil || released.Status != "released" || released.ReleasedAt == nil {
		t.Fatalf("release %+v %v", released, err)
	}
	if _, err := s.ReleasePayout(ctx, w.admin, payout.ID, "TRF-1"); err != nil {
		t.Fatalf("repeat release: %v", err)
	}
	done := s.escrow(t, ctx)
	if done["payout_payable"] != 0 || done["organizer_escrow"] != 0 {
		t.Fatalf("after release %v", done)
	}
	if _, _, err := s.RequestPayout(ctx, w.owner, w.event, "k3"); !errors.As(err, &blocked) {
		t.Fatalf("a payout after release: %v", err)
	}
	// Disputes close with the payout.
	other, _ := w.paid(t, ctx, s, "bca_va", 50000, 2000)
	if _, err := s.OpenDispute(ctx, w.tenant, other, "terlambat"); !errors.Is(err, ErrDisputeClosed) {
		t.Fatalf("dispute after payout: %v", err)
	}
}

// A failed transfer returns the reserved amount, and the organizer can ask again.
func TestPayoutFailureReturnsFunds(t *testing.T) {
	ctx, cancelCtx := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelCtx()
	s := migrated(t, ctx)
	w := newWorld(t, ctx, s, time.Now().AddDate(0, 0, -30))
	w.paid(t, ctx, s, "bca_va", 100000, 2000)
	if _, err := s.SetBankAccount(ctx, w.owner, w.org, BankAccount{BankCode: "bca", AccountNumber: "1234567890", HolderName: "EO"}); err != nil {
		t.Fatal(err)
	}
	payout, _, err := s.RequestPayout(ctx, w.owner, w.event, "a")
	if err != nil {
		t.Fatal(err)
	}
	failed, err := s.FailPayout(ctx, w.admin, payout.ID, "Rekening tidak valid")
	if err != nil || failed.Status != "failed" {
		t.Fatalf("fail %+v %v", failed, err)
	}
	if got := s.escrow(t, ctx); got["payout_payable"] != 0 || got["organizer_escrow"] != -100000 {
		t.Fatalf("funds were not returned: %v", got)
	}
	again, created, err := s.RequestPayout(ctx, w.owner, w.event, "b")
	if err != nil || !created || again.ID == payout.ID || again.Amount != 100000 {
		t.Fatalf("a new request after a failed payout %+v %v %v", again, created, err)
	}
}

func TestCheckIn(t *testing.T) {
	ctx, cancelCtx := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelCtx()
	s := migrated(t, ctx)
	w := newWorld(t, ctx, s, time.Now().Add(2*time.Hour)) // started an hour ago
	booking, _ := w.paid(t, ctx, s, "bca_va", 100000, 2000)

	// Only the tenant who paid gets a code, and it is stable.
	if _, err := s.CheckInCodeFor(ctx, w.stranger, booking); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("a stranger got a code: %v", err)
	}
	code, err := s.CheckInCodeFor(ctx, w.tenant, booking)
	if err != nil || len(code.Code) != 10 || code.QR != QRPrefix+code.Code {
		t.Fatalf("code %+v %v", code, err)
	}
	if again, _ := s.CheckInCodeFor(ctx, w.tenant, booking); again.Code != code.Code {
		t.Fatal("the code changed between requests")
	}
	unpaid := w.booking(t, ctx, s, "held", "hold", 1000)
	if _, err := s.CheckInCodeFor(ctx, w.tenant, unpaid); !errors.Is(err, ErrNotConfirmed) {
		t.Fatalf("code for an unpaid booking: %v", err)
	}

	// Staff scan, manual entry with a messy code, and the repeat scan.
	if _, err := s.CheckIn(ctx, w.tenant, w.event, CheckInInput{Code: code.Code}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("a tenant checked themselves in: %v", err)
	}
	if _, err := s.CheckIn(ctx, w.staff, w.event, CheckInInput{Code: "TOOSHORT"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("malformed code: %v", err)
	}
	if _, err := s.CheckIn(ctx, w.staff, w.event, CheckInInput{Code: "ZZZZZZZZZZ"}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("unknown code: %v", err)
	}
	messy := strings.ToLower(code.Code[:5]) + " - " + code.Code[5:]
	first, err := s.CheckIn(ctx, w.staff, w.event, CheckInInput{Code: QRPrefix + messy, Method: "qr"})
	if err != nil || first.Already || first.BusinessName != "Kopi Senja" || first.StandCode == "" {
		t.Fatalf("check-in %+v %v", first, err)
	}
	second, err := s.CheckIn(ctx, w.owner, w.event, CheckInInput{BookingID: booking})
	if err != nil || !second.Already || !second.CheckedInAt.Equal(first.CheckedInAt) {
		t.Fatalf("repeat check-in %+v %v", second, err)
	}
	if num(t, ctx, s, `SELECT count(*) FROM check_ins`) != 1 || one(t, ctx, s, `SELECT method FROM check_ins`) != "qr" {
		t.Fatal("one qr check-in expected")
	}
	if after, _ := s.CheckInCodeFor(ctx, w.tenant, booking); after.CheckedInAt == nil {
		t.Fatal("the tenant cannot see that they checked in")
	}

	// Not confirmed, and outside the period (opens 2 hours before the start, closes at the end).
	if _, err := s.CheckIn(ctx, w.staff, w.event, CheckInInput{BookingID: unpaid}); !errors.Is(err, ErrNotConfirmed) {
		t.Fatalf("check-in of an unpaid booking: %v", err)
	}
	other, _ := w.paid(t, ctx, s, "bca_va", 100000, 2000)
	exec(t, ctx, s, `UPDATE events SET starts_at = now() + interval '3 hours', ends_at = now() + interval '6 hours' WHERE id = $1`, w.event)
	var window WindowError
	if _, err := s.CheckIn(ctx, w.staff, w.event, CheckInInput{BookingID: other}); !errors.As(err, &window) || !errors.Is(err, ErrOutsideWindow) {
		t.Fatalf("too early: %v", err)
	}
	exec(t, ctx, s, `UPDATE events SET starts_at = now() + interval '119 minutes', ends_at = now() + interval '5 hours' WHERE id = $1`, w.event)
	if _, err := s.CheckIn(ctx, w.staff, w.event, CheckInInput{BookingID: other}); err != nil {
		t.Fatalf("just inside the opening window: %v", err)
	}
	third := func() string {
		id, _ := w.paid(t, ctx, s, "bca_va", 100000, 2000)
		return id
	}()
	exec(t, ctx, s, `UPDATE events SET starts_at = now() - interval '6 hours', ends_at = now() - interval '1 minute' WHERE id = $1`, w.event)
	if _, err := s.CheckIn(ctx, w.staff, w.event, CheckInInput{BookingID: third}); !errors.As(err, &window) {
		t.Fatalf("after the event: %v", err)
	}
	// A booking of another event is not found, so codes cannot be probed across events.
	otherEvent := newWorld2(t, ctx, s, w)
	if _, err := s.CheckIn(ctx, w.staff, otherEvent.event, CheckInInput{Code: code.Code}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("a code from another event: %v", err)
	}

	list, err := s.ListCheckIns(ctx, w.staff, w.event, "kopi", 50)
	if err != nil || list.Confirmed != 3 || list.CheckedIn != 2 || len(list.Items) != 3 {
		t.Fatalf("list %+v %v", list, err)
	}
	if _, err := s.ListCheckIns(ctx, w.tenant, w.event, "", 50); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("a tenant listed the guests: %v", err)
	}
	if funds, err := s.BookingFundsFor(ctx, w.tenant, booking); err != nil || funds.CheckedIn == nil || len(funds.Timeline) < 3 || funds.Timeline[len(funds.Timeline)-1].Kind != "checked_in" {
		t.Fatalf("timeline %+v %v", funds, err)
	}
}

func TestCodeNormalization(t *testing.T) {
	for in, want := range map[string]string{
		"abcde-fghjk":                   "ABCDEFGHJK",
		" tenantku:checkin:ABCDEFGHJK ": "ABCDEFGHJK",
		"ABCDE FGHJK":                   "ABCDEFGHJK",
	} {
		if got, ok := NormalizeCode(in); !ok || got != want {
			t.Errorf("%q = %q %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "ABCDEFGHJ", "ABCDEFGHJKL", "ABCDEFGHIO", "ABCDEFGHJ0"} {
		if _, ok := NormalizeCode(in); ok {
			t.Errorf("%q accepted", in)
		}
	}
	if CancellationRefund(100000) != 50000 || CancellationRefund(33333) != 16666 || CancellationRefund(1) != 0 {
		t.Error("refund share")
	}
}

func TestMain(m *testing.M) { os.Exit(m.Run()) }

func TestRupiahFormat(t *testing.T) {
	for in, want := range map[int64]string{0: "Rp0", 999: "Rp999", 1000: "Rp1.000", 20000: "Rp20.000", 1234567: "Rp1.234.567"} {
		if got := rupiah(in); got != want {
			t.Errorf("%d = %s, want %s", in, got, want)
		}
	}
}
