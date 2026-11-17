package payment

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/gin-gonic/gin"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeMidtrans serves Snap, expire, and status endpoints for the method tests.
type fakeMidtrans struct {
	mu      sync.Mutex
	snaps   []snapRequest
	expire  string // JSON body returned by the expire endpoint
	status  map[string]string
	expired []string
}

func (f *fakeMidtrans) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/snap":
		var input snapRequest
		_ = json.NewDecoder(r.Body).Decode(&input)
		f.snaps = append(f.snaps, input)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"tok","redirect_url":"https://pay.example/` + input.TransactionDetails.OrderID + `"}`))
	case strings.HasSuffix(r.URL.Path, "/expire"):
		f.expired = append(f.expired, strings.Split(r.URL.Path, "/")[1])
		_, _ = w.Write([]byte(f.expire))
	case strings.HasSuffix(r.URL.Path, "/status"):
		id := strings.Split(r.URL.Path, "/")[1]
		if body, ok := f.status[id]; ok {
			_, _ = w.Write([]byte(body))
			return
		}
		_, _ = w.Write([]byte(`{"status_code":"404","status_message":"Transaction doesn't exist."}`))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

type world struct{ user, business, category, event, slot string }

func seedWorld(t *testing.T, ctx context.Context, s Store, email string) world {
	t.Helper()
	w := world{category: one(t, ctx, s, `SELECT id FROM business_categories ORDER BY id LIMIT 1`)}
	w.user = one(t, ctx, s, `INSERT INTO users (email, password_hash) VALUES ($1, 'x') RETURNING id`, email)
	w.business = one(t, ctx, s, `INSERT INTO businesses (owner_id, category_id, name) VALUES ($1, $2, 'B') RETURNING id`, w.user, w.category)
	if _, err := s.DB.Exec(ctx, `INSERT INTO memberships (user_id, business_id, role) VALUES ($1, $2, 'owner')`, w.user, w.business); err != nil {
		t.Fatal(err)
	}
	org := one(t, ctx, s, `INSERT INTO organizations (owner_id, name) VALUES ($1, 'O') RETURNING id`, w.user)
	w.event = one(t, ctx, s, `INSERT INTO events (organization_id, created_by, name, slug, status, starts_at, ends_at,
		timezone, city, address, terms) VALUES ($1, $2, 'Pasar Malam', 'pasar', 'published', now(), now() + interval '2 hours',
		'Asia/Jakarta', 'Banda Aceh', 'St', 'T') RETURNING id`, org, w.user)
	if _, err := s.DB.Exec(ctx, `INSERT INTO event_layouts (event_id, revision, width, height, cell_meters, updated_by)
		VALUES ($1, 1, 8, 8, 1, $2)`, w.event, w.user); err != nil {
		t.Fatal(err)
	}
	w.slot = one(t, ctx, s, `INSERT INTO event_slots (event_id, code, starts_at, ends_at)
		SELECT id, 'whole', starts_at, ends_at FROM events WHERE id = $1 RETURNING id`, w.event)
	if _, err := s.DB.Exec(ctx, `INSERT INTO event_pricing_versions (event_id, revision, created_by, rules, effective_at)
		VALUES ($1, 1, $2, '{"default_base_price":30000}', now())`, w.event, w.user); err != nil {
		t.Fatal(err)
	}
	return w
}

func TestMethodSwitchFeesAndLedger(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s := migratedStore(t, ctx)
	fake := &fakeMidtrans{expire: `{"status_code":"200","transaction_status":"expire"}`, status: map[string]string{}}
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer server.Close()
	s.Provider = NewMidtrans(testKey, "sandbox")
	s.Provider.SnapURL, s.Provider.StatusURL = server.URL+"/snap", server.URL
	s.AppOrigin = "https://app.example.com"
	w := seedWorld(t, ctx, s, "m@example.com")
	// Rent 30000. The booking's fixed platform fee (1000) and provider fee (500) are replaced by the method's admin fee.
	booking := seedBooking(t, ctx, s, w.user, w.business, w.category, w.event, w.slot, "00000000-0000-4000-8000-0000000000a1")
	pay := func(key, method string, total int64) (Attempt, bool, error) {
		return s.CreateAttempt(ctx, w.user, "m@example.com", booking, key, Input{Method: method, ExpectedTotal: &total})
	}

	// Released methods: BCA VA with a flat fee and QRIS with none. Each total is exact before any attempt exists.
	options, err := s.PaymentOptions(ctx, w.user, booking)
	if err != nil || !options.Payable || options.Rent != 30000 || options.FeeBearer != "tenant" || options.Attempt != nil || len(options.Methods) != 2 {
		t.Fatalf("options %+v %v", options, err)
	}
	totals := map[string]int64{}
	for _, m := range options.Methods {
		totals[m.Code] = m.Total
	}
	if totals["bca_va"] != 34000 || totals["other_qris"] != 30000 {
		t.Fatalf("method totals %v", totals)
	}
	if _, err := s.PaymentOptions(ctx, "00000000-0000-4000-8000-0000000000ff", booking); err == nil {
		t.Fatal("another account must not read payment options")
	}

	// Unknown, closed, or stale-total requests never reach the provider.
	if _, _, err := pay("k0", "cash", 30000); !errors.Is(err, ErrUnknownMethod) {
		t.Fatalf("unknown method: %v", err)
	}
	if _, _, err := pay("k0b", "gopay", 30000); !errors.Is(err, ErrUnknownMethod) {
		t.Fatalf("closed method: %v", err)
	}
	if _, _, err := pay("k0c", "bca_va", 30000); !errors.Is(err, ErrQuoteChanged) {
		t.Fatalf("stale total: %v", err)
	}
	if len(fake.snaps) != 0 {
		t.Fatal("rejected requests reached the provider")
	}
	// A percentage method, closed by default, is priced so the platform still keeps the rent.
	if _, err := s.DB.Exec(ctx, `UPDATE payment_methods SET enabled = true WHERE code = 'qris'`); err != nil {
		t.Fatal(err)
	}

	// First attempt: the transfer fee is added, and the hosted page is limited to that method's channel.
	first, created, err := pay("k1", "bca_va", 34000)
	if err != nil || !created || first.Amount != 34000 || first.MethodFee != 4000 || first.Method != "bca_va" {
		t.Fatalf("first attempt %+v %v", first, err)
	}
	if count(t, ctx, s, `SELECT count(*) FROM payment_attempts WHERE id = $1 AND rent_amount = 30000
		AND platform_fee_amount = 0 AND provider_fee_amount = 4000 AND fee_version = 'midtrans-sandbox-v1'`, first.ID) != 1 {
		t.Fatal("attempt did not record its fee breakdown and version")
	}
	sent := fake.snaps[0]
	var sum int64
	for _, item := range sent.ItemDetails {
		sum += item.Price * int64(item.Quantity)
	}
	if sent.TransactionDetails.GrossAmount != 34000 || sum != 34000 || len(sent.ItemDetails) != 2 ||
		strings.Join(sent.EnabledPayments, ",") != "bca_va" ||
		sent.Callbacks == nil || sent.Callbacks.Finish != "https://app.example.com/tenant/bookings/"+booking+"?pembayaran=selesai" {
		t.Fatalf("snap request %+v", sent)
	}

	// Same method and total again reuses the live attempt and does not touch the provider.
	same, created, err := pay("k1b", "bca_va", 34000)
	if err != nil || created || same.ID != first.ID || len(fake.snaps) != 1 || len(fake.expired) != 0 {
		t.Fatalf("same method %+v %v", same, err)
	}

	// The provider refuses to close it (already being paid): no second attempt, nothing changes.
	fake.expire = `{"status_code":"412","status_message":"Transaction status cannot be updated."}`
	if _, _, err := pay("k2", "qris", 30212); !errors.Is(err, ErrPaymentInProgress) {
		t.Fatalf("expected in-progress refusal, got %v", err)
	}
	if got := one(t, ctx, s, `SELECT status FROM payment_attempts WHERE id = $1`, first.ID); got != "pending" || len(fake.snaps) != 1 {
		t.Fatalf("refused switch changed state: %s", got)
	}

	// Refused only because the provider already closed it: that is safe to replace, so the status lookup decides.
	fake.status[first.ID] = `{"order_id":"` + first.ID + `","status_code":"407","transaction_status":"expire","gross_amount":"34000.00","currency":"IDR","payment_type":"bank_transfer","transaction_id":"t0"}`
	closed, created, err := pay("k2b", "qris", 30212)
	if err != nil || !created || closed.Method != "qris" {
		t.Fatalf("switch over an already-closed attempt %+v %v", closed, err)
	}
	// Put the old attempt back to test the remaining paths from the same starting point.
	if _, err := s.DB.Exec(ctx, `UPDATE payment_attempts SET status = 'expired' WHERE id = $1`, closed.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(ctx, `UPDATE payment_attempts SET status = 'pending' WHERE id = $1`, first.ID); err != nil {
		t.Fatal(err)
	}
	delete(fake.status, first.ID)

	// Switching closes the old attempt and prices the new method from scratch (QRIS 0.7%, grossed up).
	fake.expire = `{"status_code":"404","status_message":"Transaction doesn't exist."}`
	second, created, err := pay("k3", "qris", 30212)
	if err != nil || !created || second.ID == first.ID || second.Amount != 30212 || second.MethodFee != 212 || second.Method != "qris" {
		t.Fatalf("switched attempt %+v %v", second, err)
	}
	if got := one(t, ctx, s, `SELECT status FROM payment_attempts WHERE id = $1`, first.ID); got != "expired" {
		t.Fatalf("old attempt %s", got)
	}
	if count(t, ctx, s, `SELECT count(*) FROM payment_attempts WHERE booking_id = $1 AND status IN ('created','pending')`, booking) != 1 {
		t.Fatal("more than one live attempt")
	}
	if got := one(t, ctx, s, `SELECT status FROM bookings WHERE id = $1`, booking); got != "payment_pending" {
		t.Fatalf("booking %s", got)
	}

	// A late expire notice for the replaced attempt must not put the booking back to held.
	n, raw := notify(first.ID, "expire", "407", "34000.00")
	if err := s.ApplyNotification(ctx, n, raw); err != nil {
		t.Fatal(err)
	}
	if got := one(t, ctx, s, `SELECT status FROM bookings WHERE id = $1`, booking); got != "payment_pending" {
		t.Fatalf("old attempt's expiry reverted booking to %s", got)
	}

	// Options now show the latest attempt so a returning payer sees where it stands.
	options, err = s.PaymentOptions(ctx, w.user, booking)
	if err != nil || options.Attempt == nil || options.Attempt.ID != second.ID || options.Attempt.CheckoutURL == "" {
		t.Fatalf("options after switch %+v %v", options, err)
	}

	// Paying with a different method than priced is flagged but the money is still booked.
	n, raw = notify(second.ID, "settlement", "200", "30212.00")
	n.PaymentType = "gopay"
	raw, _ = json.Marshal(n)
	if err := s.ApplyNotification(ctx, n, raw); err != nil {
		t.Fatal(err)
	}
	if got := one(t, ctx, s, `SELECT status FROM bookings WHERE id = $1`, booking); got != "confirmed" {
		t.Fatalf("booking %s", got)
	}
	if got := one(t, ctx, s, `SELECT confirmed_type FROM payment_attempts WHERE id = $1`, second.ID); got != "gopay" {
		t.Fatalf("confirmed type %s", got)
	}
	if count(t, ctx, s, `SELECT count(*) FROM payment_exceptions WHERE kind = 'method_mismatch'`) != 1 {
		t.Fatal("expected method_mismatch exception")
	}
	balances, err := s.Balances(ctx)
	if err != nil || balances[accountClearing] != 30212 || balances[accountEscrow] != -30000 ||
		balances[accountRevenue] != 0 || balances[accountProviderFee] != -212 {
		t.Fatalf("balances %v %v", balances, err)
	}

	// A reversal mirrors the payment once and brings every account back to zero.
	if err := s.ReversePayment(ctx, second.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.ReversePayment(ctx, second.ID); !errors.Is(err, ErrAlreadyPosted) {
		t.Fatalf("second reversal: %v", err)
	}
	if err := s.ReversePayment(ctx, first.ID); !errors.Is(err, ErrNothingToReverse) {
		t.Fatalf("reversal of an unpaid attempt: %v", err)
	}
	balances, err = s.Balances(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for account, net := range balances {
		if net != 0 {
			t.Fatalf("%s left at %d after reversal", account, net)
		}
	}
}

func TestReconcileAndLedgerKinds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s := migratedStore(t, ctx)
	fake := &fakeMidtrans{expire: `{"status_code":"200","transaction_status":"expire"}`, status: map[string]string{}}
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer server.Close()
	s.Provider = NewMidtrans(testKey, "sandbox")
	s.Provider.SnapURL, s.Provider.StatusURL = server.URL+"/snap", server.URL
	w := seedWorld(t, ctx, s, "r@example.com")
	missed := seedBooking(t, ctx, s, w.user, w.business, w.category, w.event, w.slot, "00000000-0000-4000-8000-0000000000b1")
	total := int64(34000)
	attempt, _, err := s.CreateAttempt(ctx, w.user, "r@example.com", missed, "r1", Input{Method: "bca_va", ExpectedTotal: &total})
	if err != nil {
		t.Fatal(err)
	}

	// Fresh attempts are left alone; stale ones are verified with the provider and applied like a webhook.
	report, err := s.Reconcile(ctx, time.Minute, 50)
	if err != nil || report.Checked != 0 || !report.Balanced {
		t.Fatalf("fresh pass %+v %v", report, err)
	}
	if _, err := s.DB.Exec(ctx, `UPDATE payment_attempts SET updated_at = now() - interval '10 minutes'`); err != nil {
		t.Fatal(err)
	}
	fake.status[attempt.ID] = `{"order_id":"` + attempt.ID + `","status_code":"200","transaction_status":"settlement","gross_amount":"34000.00","currency":"IDR","payment_type":"bank_transfer","transaction_id":"tx-1"}`
	report, err = s.Reconcile(ctx, time.Minute, 50)
	if err != nil || report.Checked != 1 || report.Synced != 1 || !report.Balanced || report.LedgerMissing != 0 {
		t.Fatalf("sync pass %+v %v", report, err)
	}
	if got := one(t, ctx, s, `SELECT status FROM bookings WHERE id = $1`, missed); got != "confirmed" {
		t.Fatalf("missed webhook left booking %s", got)
	}
	// A second pass changes nothing.
	again, err := s.Reconcile(ctx, time.Minute, 50)
	if err != nil || again.Checked != 0 || again.OpenExceptions != report.OpenExceptions {
		t.Fatalf("repeat pass %+v %v", again, err)
	}

	// An attempt never opened at the provider is closed only well after its expiry.
	idle := seedBooking(t, ctx, s, w.user, w.business, w.category, w.event, w.slot, "00000000-0000-4000-8000-0000000000b2")
	total = 30000
	unopened, _, err := s.CreateAttempt(ctx, w.user, "r@example.com", idle, "r2", Input{Method: "other_qris", ExpectedTotal: &total})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(ctx, `UPDATE payment_attempts SET updated_at = now() - interval '10 minutes' WHERE id = $1`, unopened.ID); err != nil {
		t.Fatal(err)
	}
	if report, err = s.Reconcile(ctx, time.Minute, 50); err != nil || report.Closed != 0 {
		t.Fatalf("closed too early %+v %v", report, err)
	}
	if _, err := s.DB.Exec(ctx, `UPDATE payment_attempts SET expires_at = now() - interval '20 minutes' WHERE id = $1`, unopened.ID); err != nil {
		t.Fatal(err)
	}
	if report, err = s.Reconcile(ctx, time.Minute, 50); err != nil || report.Closed != 1 {
		t.Fatalf("not closed %+v %v", report, err)
	}
	if got := one(t, ctx, s, `SELECT status FROM payment_attempts WHERE id = $1`, unopened.ID); got != "expired" {
		t.Fatalf("attempt %s", got)
	}

	// A paid attempt with no ledger transaction is queued once, however often reconciliation runs.
	if _, err := s.DB.Exec(ctx, `UPDATE payment_attempts SET status = 'paid', paid_at = now() WHERE id = $1`, unopened.ID); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if report, err = s.Reconcile(ctx, time.Minute, 50); err != nil {
			t.Fatal(err)
		}
	}
	if count(t, ctx, s, `SELECT count(*) FROM payment_exceptions WHERE kind = 'ledger_missing' AND status = 'open'`) != 1 {
		t.Fatal("expected exactly one open ledger_missing exception")
	}
	id := one(t, ctx, s, `SELECT id FROM payment_exceptions WHERE kind = 'ledger_missing'`)
	if err := s.ResolveException(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveException(ctx, id); err == nil {
		t.Fatal("a resolved exception cannot be resolved twice")
	}

	// Refunds and payouts are their own ledger kinds and leave escrow and clearing in step.
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := RecordRefund(ctx, tx, "00000000-0000-4000-8000-0000000000c1", 15000); err != nil {
		t.Fatal(err)
	}
	if err := RecordPayout(ctx, tx, "00000000-0000-4000-8000-0000000000c2", 15000); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if count(t, ctx, s, `SELECT count(*) FROM ledger_transactions WHERE kind IN ('refund_issued','payout_released')`) != 2 {
		t.Fatal("refund and payout were not posted as separate kinds")
	}
	balances, err := s.Balances(ctx)
	if err != nil || balances[accountEscrow] != -30000+30000 || balances[accountClearing] != 34000-30000 {
		t.Fatalf("balances after refund and payout %v %v", balances, err)
	}
}

// The webhook handler is the hosted-style entry point: signature first, then the provider's own
// status decides, so a forged or stale body cannot change a booking.
func TestWebhookHandlerTrustsProviderStatus(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s := migratedStore(t, ctx)
	fake := &fakeMidtrans{expire: `{"status_code":"200","transaction_status":"expire"}`, status: map[string]string{}}
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer server.Close()
	s.Provider = NewMidtrans(testKey, "sandbox")
	s.Provider.SnapURL, s.Provider.StatusURL = server.URL+"/snap", server.URL
	w := seedWorld(t, ctx, s, "w@example.com")
	booking := seedBooking(t, ctx, s, w.user, w.business, w.category, w.event, w.slot, "00000000-0000-4000-8000-0000000000d1")
	total := int64(34000)
	attempt, _, err := s.CreateAttempt(ctx, w.user, "w@example.com", booking, "w1", Input{Method: "bca_va", ExpectedTotal: &total})
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	router.POST("/webhooks/payments/:provider", s.webhook)
	post := func(provider string, n Notification) int {
		raw, _ := json.Marshal(n)
		req := httptest.NewRequest(http.MethodPost, "/webhooks/payments/"+provider, strings.NewReader(string(raw)))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code
	}
	status := func() string { return one(t, ctx, s, `SELECT status FROM bookings WHERE id = $1`, booking) }

	signed, _ := notify(attempt.ID, "settlement", "200", "34000.00")
	forged := signed
	forged.SignatureKey = "00"
	if code := post("midtrans", forged); code != http.StatusUnauthorized || status() != "payment_pending" {
		t.Fatalf("forged signature: %d %s", code, status())
	}
	if code := post("other", signed); code != http.StatusNotFound {
		t.Fatalf("unknown provider: %d", code)
	}
	// Signed settlement but the provider says it is still pending: the provider wins.
	fake.status[attempt.ID] = `{"order_id":"` + attempt.ID + `","status_code":"201","transaction_status":"pending","gross_amount":"34000.00","currency":"IDR","payment_type":"bank_transfer","transaction_id":"t1"}`
	if code := post("midtrans", signed); code != http.StatusNoContent || status() != "payment_pending" {
		t.Fatalf("contradicting status: %d %s", code, status())
	}
	// The provider cannot be reached: 503 so Midtrans retries, and nothing is stored as handled.
	delete(fake.status, attempt.ID)
	if code := post("midtrans", signed); code != http.StatusServiceUnavailable || status() != "payment_pending" {
		t.Fatalf("provider unavailable: %d %s", code, status())
	}
	// Settled at the provider with the right amount confirms, once, however many times it is delivered.
	fake.status[attempt.ID] = `{"order_id":"` + attempt.ID + `","status_code":"200","transaction_status":"settlement","gross_amount":"34000.00","currency":"IDR","payment_type":"bank_transfer","transaction_id":"t2"}`
	for range 3 {
		if code := post("midtrans", signed); code != http.StatusNoContent {
			t.Fatalf("settlement: %d", code)
		}
	}
	if status() != "confirmed" || count(t, ctx, s, `SELECT count(*) FROM ledger_transactions WHERE kind = 'payment_received'`) != 1 {
		t.Fatalf("settlement not applied once: %s", status())
	}
	// A wrong amount at the provider is an exception, not a payment.
	other := seedBooking(t, ctx, s, w.user, w.business, w.category, w.event, w.slot, "00000000-0000-4000-8000-0000000000d2")
	wrong, _, err := s.CreateAttempt(ctx, w.user, "w@example.com", other, "w2", Input{Method: "bca_va", ExpectedTotal: &total})
	if err != nil {
		t.Fatal(err)
	}
	fake.status[wrong.ID] = `{"order_id":"` + wrong.ID + `","status_code":"200","transaction_status":"settlement","gross_amount":"100.00","currency":"IDR","payment_type":"bank_transfer","transaction_id":"t3"}`
	bad, _ := notify(wrong.ID, "settlement", "200", "100.00")
	if code := post("midtrans", bad); code != http.StatusNoContent ||
		one(t, ctx, s, `SELECT status FROM bookings WHERE id = $1`, other) != "payment_pending" ||
		count(t, ctx, s, `SELECT count(*) FROM payment_exceptions WHERE kind = 'amount_mismatch'`) != 1 {
		t.Fatalf("wrong amount: %d", code)
	}
}

// The migrations that change what an attempt charges, in the order they run.
var paymentFeeMigration = map[string]bool{
	"00020_payment_methods.up.sql":             true,
	"00024_release_event_plans.up.sql":         true,
	"00025_booking_payment_breakdown.up.sql":   true,
	"00026_booking_payment_fee_version.up.sql": true,
	"00036_unify_payment_fee_breakdown.up.sql": true,
}

// Databases that already hold attempts must upgrade cleanly through every payment migration: the old
// booking total becomes a breakdown on the attempt, and the interim columns are retired.
func TestPaymentFeeMigrationsBackfill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s := storeWithMigrations(t, ctx, func(name string) bool { return name < "00020_" })
	w := seedWorld(t, ctx, s, "u@example.com")
	booking := seedBooking(t, ctx, s, w.user, w.business, w.category, w.event, w.slot, "00000000-0000-4000-8000-0000000000e1")
	// seedBooking holds rent 30000, platform fee 1000, provider fee 500, so the old attempt charged 31500.
	if _, err := s.DB.Exec(ctx, `INSERT INTO payment_attempts (booking_id, created_by, provider, method, status, amount, expires_at, idempotency_key, request_digest)
		VALUES ($1, $2, 'midtrans', 'snap', 'pending', 31500, now() + interval '10 minutes', 'old', decode(repeat('00', 32), 'hex'))`, booking, w.user); err != nil {
		t.Fatal(err)
	}
	purchase := one(t, ctx, s, `INSERT INTO event_plan_purchases (event_id, plan_code, purchased_by, amount) VALUES ($1, 'standard', $2, 50000) RETURNING id`, w.event, w.user)
	if _, err := s.DB.Exec(ctx, `INSERT INTO payment_attempts (purchase_id, created_by, provider, method, status, amount, expires_at, idempotency_key, request_digest)
		VALUES ($1, $2, 'midtrans', 'snap', 'paid', 50000, now() + interval '10 minutes', 'oldplan', decode(repeat('00', 32), 'hex'))`, purchase, w.user); err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir("../../migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if !strings.HasSuffix(f.Name(), ".up.sql") || !paymentFeeMigration[f.Name()] {
			continue
		}
		sql, err := os.ReadFile("../../migrations/" + f.Name())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("%s: %v", f.Name(), err)
		}
	}
	if count(t, ctx, s, `SELECT count(*) FROM payment_attempts WHERE booking_id IS NOT NULL AND rent_amount = 30000
		AND platform_fee_amount = 1000 AND provider_fee_amount = 500 AND amount = 31500`) != 1 ||
		count(t, ctx, s, `SELECT count(*) FROM payment_attempts WHERE purchase_id IS NOT NULL AND rent_amount IS NULL AND amount = 50000`) != 1 {
		t.Fatal("existing attempts were not given a breakdown")
	}
	if count(t, ctx, s, `SELECT count(*) FROM information_schema.columns WHERE table_name = 'payment_attempts'
		AND table_schema = current_schema() AND column_name IN ('base_amount', 'method_fee')`) != 0 {
		t.Fatal("interim fee columns were not retired")
	}
	if count(t, ctx, s, `SELECT count(*) FROM payment_methods WHERE enabled AND code IN ('bca_va', 'other_qris')`) != 2 ||
		count(t, ctx, s, `SELECT count(*) FROM payment_methods WHERE enabled`) != 2 {
		t.Fatal("only BCA VA and QRIS should be open after upgrade")
	}
}
