package events

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// E09-05: cancelling an event with paid, pending, and held bookings no longer stops at a 409. Holds and
// pending payments are closed, and paid bookings are refunded half their rent.
func TestCancelEventSettlesBookings(t *testing.T) {
	url := os.Getenv("TENANTKU_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TENANTKU_TEST_DATABASE_URL to run the PostgreSQL cancellation test")
	}
	ctx, cancelCtx := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelCtx()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	schema := pgx.Identifier{"cancel_" + strings.ToLower(rand.Text())}.Sanitize()
	t.Cleanup(func() {
		c, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		defer conn.Close(c)
		conn.Exec(c, fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", schema))
	})
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(url)
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
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	one := func(query string, args ...any) string {
		t.Helper()
		var v string
		if err := pool.QueryRow(ctx, query, args...).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	owner := one(`INSERT INTO users (email, password_hash) VALUES ('o@e.com', 'x') RETURNING id`)
	tenant := one(`INSERT INTO users (email, password_hash) VALUES ('t@e.com', 'x') RETURNING id`)
	org := one(`INSERT INTO organizations (owner_id, name) VALUES ($1, 'EO') RETURNING id`, owner)
	exec(`INSERT INTO memberships (user_id, organization_id, role) VALUES ($1, $2, 'owner')`, owner, org)
	category := one(`SELECT id FROM business_categories LIMIT 1`)
	business := one(`INSERT INTO businesses (owner_id, category_id, name) VALUES ($1, $2, 'B') RETURNING id`, tenant, category)
	event := one(`INSERT INTO events (organization_id, created_by, name, slug, status, starts_at, ends_at, timezone, city, address, terms)
		VALUES ($1, $2, 'E', 'e', 'published', now() + interval '1 day', now() + interval '2 days', 'Asia/Jakarta', 'Banda Aceh', 'St', 'T') RETURNING id`, org, owner)
	exec(`INSERT INTO event_layouts (event_id, revision, width, height, cell_meters, updated_by) VALUES ($1, 1, 8, 8, 1, $2)`, event, owner)
	slot := one(`INSERT INTO event_slots (event_id, code, starts_at, ends_at) SELECT id, 'whole', starts_at, ends_at FROM events WHERE id = $1 RETURNING id`, event)
	exec(`INSERT INTO event_pricing_versions (event_id, revision, created_by, rules, effective_at) VALUES ($1, 1, $2, '{"default_base_price":100000}', now())`, event, owner)

	booking := func(n int, status, allocation string) string {
		stand := fmt.Sprintf("00000000-0000-4000-8000-%012d", n)
		exec(`INSERT INTO stands (id, event_id, code, x, y, width, height) VALUES ($1::uuid, $2, $3, $4, 0, 1, 1)`, stand, event, fmt.Sprintf("S%d", n), n)
		id := one(`INSERT INTO bookings (business_id, event_id, category_id, created_by, status, expires_at, price_revision, rent, platform_fee, provider_fee,
			terms_snapshot, refund_policy_version, refund_policy_snapshot) VALUES ($1, $2, $3, $4, $5, now() + interval '1 hour', 1, 100000, 0, 0, 't', 'v1', '{}') RETURNING id`,
			business, event, category, tenant, status)
		exec(`INSERT INTO allocations (booking_id, event_id, event_slot_id, stand_id, status) VALUES ($1, $2, $3, $4, $5)`, id, event, slot, stand, allocation)
		return id
	}
	hold := booking(1, "held", "hold")
	paid := booking(2, "confirmed", "reserved")
	attempt := one(`INSERT INTO payment_attempts (booking_id, created_by, provider, method, status, amount, expires_at, idempotency_key, request_digest, paid_at,
		rent_amount, platform_fee_amount, provider_fee_amount) VALUES ($1, $2, 'midtrans', 'bca_va', 'paid', 102000, now() + interval '1 hour', 'k', decode(repeat('00', 32), 'hex'),
		now(), 100000, 0, 2000) RETURNING id`, paid, tenant)
	tx := one(`INSERT INTO ledger_transactions (kind, ref_type, ref_id) VALUES ('payment_received', 'payment_attempt', $1) RETURNING id`, attempt)
	exec(`INSERT INTO ledger_entries (transaction_id, account, direction, amount) VALUES ($1, 'provider_clearing', 'debit', 102000),
		($1, 'organizer_escrow', 'credit', 100000), ($1, 'provider_fee_payable', 'credit', 2000)`, tx)

	store := Store{DB: pool}
	if _, err := store.cancel(ctx, tenant, event, "bukan pemilik"); err == nil {
		t.Fatal("a tenant cancelled the event")
	}
	data, err := store.cancel(ctx, owner, event, "Cuaca buruk")
	if err != nil || data.Status != "cancelled" {
		t.Fatalf("cancel with paid and held bookings: %+v %v", data, err)
	}
	if one(`SELECT status FROM bookings WHERE id = $1`, hold) != "cancelled" || one(`SELECT status FROM bookings WHERE id = $1`, paid) != "cancelled" ||
		one(`SELECT status FROM allocations WHERE booking_id = $1`, paid) != "released" {
		t.Fatal("bookings were not closed")
	}
	if one(`SELECT amount::text FROM refunds WHERE booking_id = $1`, paid) != "50000" ||
		one(`SELECT status FROM refunds WHERE booking_id = $1`, paid) != "requested" {
		t.Fatal("the paid booking was not owed half its rent")
	}
	// The notification worker picks the cancellation up from the booking history.
	if one(`SELECT count(*)::text FROM booking_status_history WHERE reason = 'event_cancelled'`) != "2" {
		t.Fatal("tenants would not be notified")
	}
	// The same request again returns the recorded cancellation without refunding twice.
	if _, err := store.cancel(ctx, owner, event, "Cuaca buruk"); err != nil || one(`SELECT count(*)::text FROM refunds`) != "1" {
		t.Fatalf("repeat cancellation: %v", err)
	}
}
