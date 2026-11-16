package booking

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	testStandOne   = "00000000-0000-4000-8000-000000000001"
	testStandTwo   = "00000000-0000-4000-8000-000000000002"
	testStandThree = "00000000-0000-4000-8000-000000000003"
)

type testOffers struct {
	offer Offer
}

func (o testOffers) ResolveOffer(_ context.Context, _ pgx.Tx, input OfferRequest) (Offer, error) {
	if input.StandID != testStandOne && input.StandID != testStandTwo && input.StandID != testStandThree {
		return Offer{}, ErrNotBookable
	}
	return o.offer, nil
}

func TestHoldTransactions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s := migratedBookingStore(t, ctx)
	var category string
	if err := s.DB.QueryRow(ctx, `SELECT id FROM business_categories ORDER BY id LIMIT 1`).Scan(&category); err != nil {
		t.Fatal(err)
	}
	first, firstBusiness := createTestBusiness(t, ctx, s, category, "one@example.com")
	second, secondBusiness := createTestBusiness(t, ctx, s, category, "two@example.com")

	org := insertID(t, ctx, s, `INSERT INTO organizations (owner_id, name) VALUES ($1, 'Sample') RETURNING id`, first)
	event := insertID(t, ctx, s, `INSERT INTO events
		(organization_id, created_by, name, slug, status, starts_at, ends_at, timezone, city, address, terms)
		VALUES ($1, $2, 'Event', 'event', 'published', now(), now() + interval '2 hours',
			'Asia/Jakarta', 'Banda Aceh', 'Street', 'Original terms') RETURNING id`, org, first)
	slot := insertID(t, ctx, s, `INSERT INTO event_slots (event_id, code, starts_at, ends_at)
		SELECT id, 'whole-event', starts_at, ends_at FROM events WHERE id = $1 RETURNING id`, event)
	createTestStands(t, ctx, s, event, first)
	execBookingSQL(t, ctx, s, `INSERT INTO event_pricing_versions (event_id, revision, created_by, rules, effective_at)
		VALUES ($1, 1, $2, '{"default_base_price":30000}', now())`, event, first)

	offers := testOffers{Offer{
		PriceRevision: 1,
		Rent:          30_000,
		PlatformFee:   1_000,
		ProviderFee:   500,
		PolicyVersion: "test-policy",
		RefundPolicy:  json.RawMessage(`{"test": true}`),
	}}
	settings := HoldSettings{Lifetime: 15 * time.Minute, MaxActive: 1}
	type request struct {
		actor, business, stand, key string
	}
	input := func(r request) HoldInput {
		return HoldInput{BusinessID: r.business, EventID: event, StandID: r.stand, EventSlotID: slot, AcceptedPolicyVersion: "test-policy"}
	}
	a := request{first, firstBusiness, testStandOne, "first-claim"}
	b := request{second, secondBusiness, testStandOne, "second-claim"}
	type claim struct {
		request
		booking Booking
		err     error
	}
	raceClaims := func(a, b request, provider testOffers) [2]claim {
		results := make(chan claim, 2)
		start := make(chan struct{})
		for _, req := range []request{a, b} {
			go func() {
				<-start
				result, err := s.CreateHold(ctx, req.actor, req.key, input(req), settings, provider)
				results <- claim{req, result, err}
			}()
		}
		close(start)
		return [2]claim{<-results, <-results}
	}
	assertOne := func(results [2]claim, conflict error) (claim, claim) {
		t.Helper()
		winner, loser := results[0], results[1]
		if winner.err != nil {
			winner, loser = loser, winner
		}
		if winner.err != nil || !errors.Is(loser.err, conflict) {
			t.Fatalf("want one winner and %v; got %v and %v", conflict, winner.err, loser.err)
		}
		return winner, loser
	}
	winner, loser := assertOne(raceClaims(a, b, offers), ErrStandUnavailable)
	if winner.booking.Price.Total != 31_500 || winner.booking.Status != "held" {
		t.Fatalf("unexpected booking: %+v", winner.booking)
	}
	if _, err := s.detail(ctx, loser.actor, winner.booking.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cross-business lookup returned %v", err)
	}
	snapshotTerms := ""
	if err := s.DB.QueryRow(ctx, `SELECT terms_snapshot FROM bookings WHERE id = $1`, winner.booking.ID).Scan(&snapshotTerms); err != nil || snapshotTerms != "Original terms" {
		t.Fatalf("terms snapshot %q, error %v", snapshotTerms, err)
	}
	changed := offers
	changed.offer.Rent = 99_000
	replayed, err := s.CreateHold(ctx, winner.actor, winner.key, input(winner.request), settings, changed)
	if err != nil || replayed.ID != winner.booking.ID || replayed.Price != winner.booking.Price {
		t.Fatalf("replayed booking %+v, error %v", replayed, err)
	}
	altered := input(winner.request)
	altered.StandID = testStandTwo
	if _, err := s.CreateHold(ctx, winner.actor, winner.key, altered, settings, offers); !errors.Is(err, ErrIdempotencyMismatch) {
		t.Fatalf("expected mismatched payload, got %v", err)
	}
	if _, err := s.CreateHold(ctx, winner.actor, "extra", altered, settings, offers); !errors.Is(err, ErrHoldLimit) {
		t.Fatalf("expected active hold limit, got %v", err)
	}

	execBookingSQL(t, ctx, s, `UPDATE bookings SET expires_at = clock_timestamp() - interval '1 second' WHERE id = $1`, winner.booking.ID)
	projected, err := s.detail(ctx, winner.actor, winner.booking.ID)
	if err != nil || projected.Status != "expired" {
		t.Fatalf("expected expired read, got %+v, %v", projected, err)
	}
	reclaimed, err := s.CreateHold(ctx, loser.actor, loser.key, input(loser.request), settings, offers)
	if err != nil || reclaimed.ID == winner.booking.ID {
		t.Fatalf("stand should be reclaimable: %+v, %v", reclaimed, err)
	}
	execBookingSQL(t, ctx, s, `UPDATE bookings SET expires_at = clock_timestamp() - interval '1 second' WHERE id = $1`, reclaimed.ID)
	if expired, err := s.Expire(ctx, 1); err != nil || expired != 1 {
		t.Fatalf("expected one expiry, got %d, %v", expired, err)
	}
	if expired, err := s.Expire(ctx, 1); err != nil || expired != 0 {
		t.Fatalf("duplicate expiry: %d, %v", expired, err)
	}
	var expiryHistory int
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM booking_status_history WHERE to_status = 'expired'`).Scan(&expiryHistory); err != nil || expiryHistory != 2 {
		t.Fatalf("expiry history count = %d, err = %v", expiryHistory, err)
	}

	quota := 1
	limited := offers
	limited.offer.EventQuota = &quota
	a.stand, a.key = testStandTwo, "quota-first"
	b.stand, b.key = testStandThree, "quota-second"
	assertOne(raceClaims(a, b, limited), ErrQuotaFull)
}

func createTestStands(t *testing.T, ctx context.Context, s Store, event, organizer string) {
	t.Helper()
	execBookingSQL(t, ctx, s, `INSERT INTO event_layouts (event_id, revision, width, height, cell_meters, updated_by)
		VALUES ($1, 1, 10, 10, 1, $2)`, event, organizer)
	execBookingSQL(t, ctx, s, `INSERT INTO stands (id, event_id, code, x, y, width, height)
		VALUES ($2, $1, 'A1', 1, 1, 1, 1), ($3, $1, 'A2', 2, 1, 1, 1), ($4, $1, 'A3', 3, 1, 1, 1)`,
		event, testStandOne, testStandTwo, testStandThree)
}

func createTestBusiness(t *testing.T, ctx context.Context, s Store, category, email string) (string, string) {
	t.Helper()
	user := insertID(t, ctx, s, `INSERT INTO users (email, password_hash) VALUES ($1, 'fixture') RETURNING id`, email)
	business := insertID(t, ctx, s, `INSERT INTO businesses (owner_id, category_id, name)
		VALUES ($1, $2, 'Business') RETURNING id`, user, category)
	execBookingSQL(t, ctx, s, `INSERT INTO memberships (user_id, business_id, role) VALUES ($1, $2, 'owner')`, user, business)
	return user, business
}

func insertID(t *testing.T, ctx context.Context, s Store, query string, args ...any) string {
	t.Helper()
	var id string
	if err := s.DB.QueryRow(ctx, query, args...).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func execBookingSQL(t *testing.T, ctx context.Context, s Store, query string, args ...any) {
	t.Helper()
	if _, err := s.DB.Exec(ctx, query, args...); err != nil {
		t.Fatal(err)
	}
}

func migratedBookingStore(t *testing.T, ctx context.Context) Store {
	t.Helper()
	databaseURL := os.Getenv("TENANTKU_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set TENANTKU_TEST_DATABASE_URL to run the PostgreSQL booking test")
	}
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	schema := pgx.Identifier{"booking_test_" + strings.ToLower(rand.Text())}.Sanitize()
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		defer conn.Close(cleanup)
		if _, err := conn.Exec(cleanup, fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", schema)); err != nil {
			t.Error(err)
		}
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
	s := Store{DB: pool}
	const directory = "../../migrations"
	migrations, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations {
		if !strings.HasSuffix(migration.Name(), ".up.sql") {
			continue
		}
		// Event cancellation uses a privileged function fixed to the deployment's
		// public schema. These hold tests only migrate a disposable schema.
		if migration.Name() == "00013_cancel_unpaid_event_bookings.up.sql" {
			continue
		}
		sql, err := os.ReadFile(filepath.Join(directory, migration.Name()))
		if err != nil {
			t.Fatal(err)
		}
		execBookingSQL(t, ctx, s, string(sql))
	}
	return s
}
