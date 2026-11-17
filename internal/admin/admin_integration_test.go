package admin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"kitakarsa.com/tenantku/backend/internal/platform/adminperm"
	"kitakarsa.com/tenantku/backend/internal/platform/testdb"
)

type fixture struct {
	store          Store
	pool           *pgxpool.Pool
	world          testdb.World
	all, auditOnly string // admin accounts: every permission, and only audit.read
	attempt        string
}

type recheckStub struct{ calls []string }

func (r *recheckStub) Recheck(_ context.Context, id string) error {
	r.calls = append(r.calls, id)
	return nil
}

func newFixture(t *testing.T, ctx context.Context) fixture {
	t.Helper()
	pool := testdb.Migrated(t, ctx, "admin")
	f := fixture{store: Store{DB: pool}, pool: pool, world: testdb.NewWorld(t, ctx, pool, "payment_pending", false)}
	f.all = testdb.User(t, ctx, pool, "admin")
	f.auditOnly = testdb.User(t, ctx, pool, "auditor")
	testdb.Exec(t, ctx, pool, `INSERT INTO platform_admins (user_id) VALUES ($1)`, f.all)
	testdb.Exec(t, ctx, pool, `INSERT INTO platform_admins (user_id, permissions) VALUES ($1, ARRAY['audit.read'])`, f.auditOnly)
	// A payment attempt made under the current Rp4.000 virtual account fee.
	f.attempt = testdb.Scalar(t, ctx, pool, `INSERT INTO payment_attempts (booking_id, created_by, provider, method, status, amount, expires_at,
		idempotency_key, request_digest, rent_amount, platform_fee_amount, provider_fee_amount, fee_version)
		VALUES ($1, $2, 'midtrans', 'bca_va', 'pending', 36000, now() + interval '1 hour', 'k1', decode(repeat('00', 32), 'hex'),
		30000, 2000, 4000, 'midtrans-sandbox-v1') RETURNING id`, f.world.Booking, f.world.Tenant)
	return f
}

// E16-01/06: each admin does only what the permissions on its row allow, checked inside the transaction.
func TestAdminPermissionBoundary(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f := newFixture(t, ctx)
	outsider := testdb.User(t, ctx, f.pool, "outsider")

	if perms, err := adminperm.Of(ctx, f.pool, f.auditOnly); err != nil || len(perms) != 1 || perms[0] != adminperm.AuditRead {
		t.Fatalf("narrow admin permissions: %v %v", perms, err)
	}
	if perms, _ := adminperm.Of(ctx, f.pool, outsider); perms != nil {
		t.Fatalf("a normal account holds admin permissions: %v", perms)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE platform_admins SET permissions = ARRAY['root'] WHERE user_id = $1`, f.all); err == nil {
		t.Fatal("an unknown permission was stored")
	}
	if _, err := f.pool.Exec(ctx, `UPDATE platform_admins SET permissions = '{}' WHERE user_id = $1`, f.all); err == nil {
		t.Fatal("an admin without any permission was stored")
	}

	input := methodInput{Enabled: ptr(false), Reason: "uji"}
	for name, actor := range map[string]string{"audit-only admin": f.auditOnly, "normal account": outsider} {
		if _, err := f.store.updateMethod(ctx, actor, "bca_va", input, "req"); !errors.Is(err, errForbidden) {
			t.Errorf("%s changed a fee: %v", name, err)
		}
		if _, err := f.store.setVerification(ctx, actor, f.world.Organization, "verified", "uji", "req"); !errors.Is(err, errForbidden) {
			t.Errorf("%s verified an organizer: %v", name, err)
		}
		if _, _, err := f.store.reverseLedger(ctx, actor, f.world.Booking, "uji", "req"); !errors.Is(err, errForbidden) {
			t.Errorf("%s reversed the ledger: %v", name, err)
		}
		if _, err := f.store.resolveException(ctx, actor, f.world.Booking, "uji", "req"); !errors.Is(err, errForbidden) {
			t.Errorf("%s resolved an exception: %v", name, err)
		}
	}
	if testdb.Scalar(t, ctx, f.pool, `SELECT enabled::text FROM payment_methods WHERE code = 'bca_va'`) != "true" ||
		testdb.Scalar(t, ctx, f.pool, `SELECT count(*)::text FROM admin_audit`) != "0" {
		t.Fatal("a refused change left a trace")
	}
	if _, err := f.store.updateMethod(ctx, f.all, "bca_va", input, "req"); err != nil {
		t.Fatalf("a full admin was refused: %v", err)
	}
}

func ptr[T any](v T) *T { return &v }

// E16-03/06: a new fee applies to later attempts only, the change is audited with before and after, and the same
// request sent again changes and records nothing.
func TestMethodFeeIsProspectiveAndAudited(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f := newFixture(t, ctx)
	change := methodInput{FeeFlat: ptr(int64(5000)), FeeVersion: ptr("midtrans-contract-2026-12"), FeeSource: ptr("contract"), Reason: "Kontrak baru"}

	// A fee amount without a new fee version is refused, so charges stay attributable.
	noVersion := change
	noVersion.FeeVersion = nil
	var invalid fieldError
	if _, err := f.store.updateMethod(ctx, f.all, "bca_va", noVersion, "req-0"); !errors.As(err, &invalid) || invalid["fee_version"] == "" {
		t.Fatalf("fee change without a version: %v", err)
	}

	updated, err := f.store.updateMethod(ctx, f.all, "bca_va", change, "req-1")
	if err != nil || updated.FeeFlat != 5000 || updated.FeeVersion != "midtrans-contract-2026-12" || updated.FeeSource != "contract" {
		t.Fatalf("update: %+v %v", updated, err)
	}
	// The attempt created before the change keeps the fee and version it was charged under.
	if testdb.Scalar(t, ctx, f.pool, `SELECT provider_fee_amount || '/' || fee_version FROM payment_attempts WHERE id = $1`, f.attempt) != "4000/midtrans-sandbox-v1" {
		t.Fatal("an existing attempt was repriced")
	}

	// Sending it again is a no-op: same row, no second audit entry.
	again, err := f.store.updateMethod(ctx, f.all, "bca_va", change, "req-2")
	if err != nil || again.UpdatedAt != updated.UpdatedAt {
		t.Fatalf("repeat: %+v %v", again, err)
	}
	if testdb.Scalar(t, ctx, f.pool, `SELECT count(*)::text FROM admin_audit WHERE action = 'payment_method.update'`) != "1" {
		t.Fatal("a retry wrote a second audit entry")
	}

	var before, after json.RawMessage
	var actor, reason, request string
	if err := f.pool.QueryRow(ctx, `SELECT actor_id::text, reason, request_id, before, after FROM admin_audit`).Scan(&actor, &reason, &request, &before, &after); err != nil {
		t.Fatal(err)
	}
	if actor != f.all || reason != "Kontrak baru" || request != "req-1" ||
		!strings.Contains(string(before), `"fee_flat": 4000`) || !strings.Contains(string(after), `"fee_flat": 5000`) ||
		!strings.Contains(string(before), "midtrans-sandbox-v1") || !strings.Contains(string(after), "midtrans-contract-2026-12") {
		t.Fatalf("audit trail: %s %s %s %s %s", actor, reason, request, before, after)
	}
	if _, err := f.store.updateMethod(ctx, f.all, "no_such_method", methodInput{Enabled: ptr(true), Reason: "x"}, "r"); err == nil {
		t.Fatal("unknown method accepted")
	}
}

// Audit snapshots never carry secrets.
func TestAuditRedactsSecrets(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f := newFixture(t, ctx)
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := writeAudit(ctx, tx, auditRecord{actor: f.all, action: "bank.update", resourceType: "organization", reason: "uji", requestID: "r",
		before: map[string]any{"account_number": "1234567890", "bank": "BCA"}, after: map[string]any{"server_key": "SB-Mid-secret"}}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	stored := testdb.Scalar(t, ctx, f.pool, `SELECT before::text || after::text FROM admin_audit`)
	if strings.Contains(stored, "1234567890") || strings.Contains(stored, "SB-Mid-secret") || !strings.Contains(stored, "BCA") {
		t.Fatalf("audit snapshot: %s", stored)
	}
	// The log is append-only for the API role's operations: the store never updates or deletes it.
}

func planInputs(price int64) []planInput {
	return []planInput{{"mini", "Mini", 20, price}, {"large", "Large", 100, price * 3}}
}

// E16-03: a plan price list is added for a future time; the list in force and past purchases are untouched, and a retry
// neither duplicates the version nor writes a second audit entry.
func TestPlanPriceVersions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f := newFixture(t, ctx)
	current, err := f.store.planCatalog(ctx)
	if err != nil {
		t.Fatal(err)
	}

	future := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Microsecond)
	input := versionInput{EffectiveAt: future, CustomPricePerTenant: 6000, Plans: planInputs(200000), Reason: "Harga baru"}
	if fields := validateVersion(&input, time.Now()); fields != nil {
		t.Fatalf("valid input refused: %v", fields)
	}
	past := input
	past.EffectiveAt = time.Now().Add(-time.Hour)
	free := input
	free.Plans = planInputs(0)
	dup := input
	dup.Plans = []planInput{{"mini", "Mini", 20, 1}, {"mini", "Mini", 30, 2}}
	for name, bad := range map[string]versionInput{"past": past, "free": free, "duplicate code": dup} {
		if validateVersion(&bad, time.Now()) == nil {
			t.Errorf("%s input accepted", name)
		}
	}

	created, isNew, err := f.store.publishVersion(ctx, f.all, input, "req-1")
	if err != nil || !isNew || len(created.Plans) != 2 || created.Plans[1].Price != 600000 {
		t.Fatalf("publish: %+v new=%v %v", created, isNew, err)
	}
	// Nothing changes until the effective time.
	after, err := f.store.planCatalog(ctx)
	if err != nil || after.VersionID != current.VersionID || len(after.Plans) != len(current.Plans) || after.Plans[0].Price != current.Plans[0].Price {
		t.Fatalf("the price list in force changed early: %+v %v", after, err)
	}
	versions, err := f.store.planVersions(ctx)
	if err != nil || len(versions) != 2 || versions[0].ID != created.ID || versions[0].Current || !versions[1].Current {
		t.Fatalf("versions: %+v %v", versions, err)
	}

	// Retry: same list, same time.
	retry, isNew, err := f.store.publishVersion(ctx, f.all, input, "req-2")
	if err != nil || isNew || retry.ID != created.ID {
		t.Fatalf("retry: %+v new=%v %v", retry, isNew, err)
	}
	other := input
	other.Plans = planInputs(250000)
	if _, _, err := f.store.publishVersion(ctx, f.all, other, "req-3"); !errors.Is(err, errConflict) {
		t.Fatalf("a different list at the same time: %v", err)
	}
	if testdb.Scalar(t, ctx, f.pool, `SELECT count(*)::text FROM plan_price_versions`) != "2" ||
		testdb.Scalar(t, ctx, f.pool, `SELECT count(*)::text FROM admin_audit WHERE action = 'plan_prices.publish'`) != "1" {
		t.Fatal("a retry duplicated the version or its audit entry")
	}
	var before, afterJSON string
	if err := f.pool.QueryRow(ctx, `SELECT before::text, after::text FROM admin_audit`).Scan(&before, &afterJSON); err != nil ||
		!strings.Contains(before, `"price": 180000`) || !strings.Contains(afterJSON, `"price": 200000`) {
		t.Fatalf("audit before/after: %s | %s | %v", before, afterJSON, err)
	}
	if _, _, err := f.store.publishVersion(ctx, f.auditOnly, other, "r"); !errors.Is(err, errForbidden) {
		t.Fatalf("audit-only admin published prices: %v", err)
	}
}

// E16-02/03: an exception is closed once; a payment re-check runs the provider path and is audited.
func TestExceptionsAndRecheck(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f := newFixture(t, ctx)
	exception := testdb.Scalar(t, ctx, f.pool, `INSERT INTO payment_exceptions (attempt_id, booking_id, kind, detail)
		VALUES ($1, $2, 'late_payment', '{"note":"x"}') RETURNING id`, f.attempt, f.world.Booking)

	open, err := f.store.exceptions(ctx, pageFilter{Status: "open", Limit: 10})
	if err != nil || len(open) != 1 || open[0].ID != exception {
		t.Fatalf("open queue: %+v %v", open, err)
	}
	first, err := f.store.resolveException(ctx, f.all, exception, "Dikembalikan manual", "req-1")
	if err != nil || first.Status != "resolved" || first.ResolutionNote == nil || *first.ResolutionNote != "Dikembalikan manual" {
		t.Fatalf("resolve: %+v %v", first, err)
	}
	if _, err := f.store.resolveException(ctx, f.all, exception, "Catatan lain", "req-2"); err != nil {
		t.Fatal(err)
	}
	if testdb.Scalar(t, ctx, f.pool, `SELECT count(*)::text FROM admin_audit WHERE action = 'payment_exception.resolve'`) != "1" ||
		testdb.Scalar(t, ctx, f.pool, `SELECT resolution_note FROM payment_exceptions`) != "Dikembalikan manual" {
		t.Fatal("closing twice changed the note or wrote a second audit entry")
	}
	if left, _ := f.store.exceptions(ctx, pageFilter{Status: "open", Limit: 10}); len(left) != 0 {
		t.Fatalf("closed exception still open: %+v", left)
	}

	// Re-check: needs a provider, the right permission, and an existing attempt.
	if _, err := f.store.recheckPayment(ctx, f.all, f.attempt, "cek", "r"); !errors.Is(err, errUnavailable) {
		t.Fatalf("no provider: %v", err)
	}
	stub := &recheckStub{}
	f.store.Payments = stub
	if _, err := f.store.recheckPayment(ctx, f.auditOnly, f.attempt, "cek", "r"); !errors.Is(err, errForbidden) || len(stub.calls) != 0 {
		t.Fatalf("recheck without permission: %v calls=%v", err, stub.calls)
	}
	row, err := f.store.recheckPayment(ctx, f.all, f.attempt, "Webhook tidak masuk", "req-3")
	if err != nil || row.ID != f.attempt || len(stub.calls) != 1 || stub.calls[0] != f.attempt {
		t.Fatalf("recheck: %+v calls=%v %v", row, stub.calls, err)
	}
	if testdb.Scalar(t, ctx, f.pool, `SELECT count(*)::text FROM admin_audit WHERE action = 'payment.recheck' AND request_id = 'req-3'`) != "1" {
		t.Fatal("recheck was not audited")
	}
}

// E16-02: booking, payment and event search find rows by ID and by name.
func TestAdminSearch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f := newFixture(t, ctx)
	for query, want := range map[string]int{"": 1, strings.ToUpper(f.world.Booking): 1, "kopi senja": 1, "bazar": 1, "tidak ada": 0} {
		rows, err := f.store.bookings(ctx, pageFilter{Query: query, Limit: 10})
		if err != nil || len(rows) != want {
			t.Errorf("bookings %q: %d rows, want %d (%v)", query, len(rows), want, err)
		}
	}
	rows, err := f.store.bookings(ctx, pageFilter{Query: f.world.Booking, Limit: 10})
	if err != nil || rows[0].PaymentStatus == nil || *rows[0].PaymentStatus != "pending" || *rows[0].PaymentAmount != 36000 {
		t.Fatalf("booking payment summary: %+v %v", rows, err)
	}
	payments, err := f.store.payments(ctx, pageFilter{Query: f.attempt, Limit: 10})
	if err != nil || len(payments) != 1 || payments[0].Kind != "booking" || payments[0].ProviderFee != 4000 || *payments[0].FeeVersion != "midtrans-sandbox-v1" {
		t.Fatalf("payments: %+v %v", payments, err)
	}
	if rows, _ := f.store.payments(ctx, pageFilter{Status: "paid", Limit: 10}); len(rows) != 0 {
		t.Fatalf("status filter: %+v", rows)
	}
	events, err := f.store.events(ctx, pageFilter{Query: "pasar malam", Limit: 10})
	if err != nil || len(events) != 1 || events[0].Bookings != 1 {
		t.Fatalf("events: %+v %v", events, err)
	}
}

// E16-06: a balance is corrected by a reversing transaction, once, and the ledger still balances.
func TestLedgerReversal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f := newFixture(t, ctx)
	transaction := testdb.Scalar(t, ctx, f.pool, `INSERT INTO ledger_transactions (kind, ref_type, ref_id) VALUES ('payment_received', 'payment_attempt', $1) RETURNING id`, f.attempt)
	testdb.Exec(t, ctx, f.pool, `INSERT INTO ledger_entries (transaction_id, account, direction, amount) VALUES
		($1, 'provider_clearing', 'debit', 36000), ($1, 'organizer_escrow', 'credit', 30000), ($1, 'platform_revenue', 'credit', 2000),
		($1, 'provider_fee_payable', 'credit', 4000)`, transaction)

	reversal, created, err := f.store.reverseLedger(ctx, f.all, transaction, "Pembayaran dibatalkan provider", "req-1")
	if err != nil || !created || reversal.Reverses != transaction || len(reversal.Entries) != 4 {
		t.Fatalf("reverse: %+v created=%v %v", reversal, created, err)
	}
	balance := map[string]int64{}
	rows, err := f.pool.Query(ctx, `SELECT account, CASE direction WHEN 'debit' THEN amount ELSE -amount END FROM ledger_entries`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var account string
		var amount int64
		if err := rows.Scan(&account, &amount); err != nil {
			t.Fatal(err)
		}
		balance[account] += amount
	}
	rows.Close()
	for account, amount := range balance {
		if amount != 0 {
			t.Errorf("%s is %d after the correction", account, amount)
		}
	}
	// Once only: repeats return the same reversal; a reversal cannot be reversed.
	again, created, err := f.store.reverseLedger(ctx, f.all, transaction, "Coba lagi", "req-2")
	if err != nil || created || again.ID != reversal.ID {
		t.Fatalf("repeat: %+v created=%v %v", again, created, err)
	}
	if _, _, err := f.store.reverseLedger(ctx, f.all, reversal.ID, "salah", "req-3"); !errors.Is(err, errNotReversible) {
		t.Fatalf("reversing a reversal: %v", err)
	}
	if testdb.Scalar(t, ctx, f.pool, `SELECT count(*)::text FROM ledger_transactions WHERE kind = 'reversal'`) != "1" ||
		testdb.Scalar(t, ctx, f.pool, `SELECT count(*)::text FROM admin_audit WHERE action = 'ledger.reverse'`) != "1" {
		t.Fatal("a retry doubled the correction or its audit entry")
	}
	// The original rows are untouched: the ledger only ever grows.
	if _, err := f.pool.Exec(ctx, `UPDATE ledger_entries SET amount = 1`); err == nil {
		t.Fatal("ledger rows can be edited")
	}
}
