package booking

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestCuratedApplications(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s := migratedBookingStore(t, ctx)
	var category string
	if err := s.DB.QueryRow(ctx, `SELECT id FROM business_categories ORDER BY id LIMIT 1`).Scan(&category); err != nil {
		t.Fatal(err)
	}
	organizer := insertID(t, ctx, s, `INSERT INTO users (email, password_hash) VALUES ('org@example.com', 'x') RETURNING id`)
	stranger := insertID(t, ctx, s, `INSERT INTO users (email, password_hash) VALUES ('stranger@example.com', 'x') RETURNING id`)
	tenantA, businessA := createTestBusiness(t, ctx, s, category, "a@example.com")
	tenantB, businessB := createTestBusiness(t, ctx, s, category, "b@example.com")
	org := insertID(t, ctx, s, `INSERT INTO organizations (owner_id, name) VALUES ($1, 'Org') RETURNING id`, organizer)
	execBookingSQL(t, ctx, s, `INSERT INTO memberships (user_id, organization_id, role) VALUES ($1, $2, 'owner')`, organizer, org)
	event := insertID(t, ctx, s, `INSERT INTO events
		(organization_id, created_by, name, slug, status, starts_at, ends_at, timezone, city, address, terms)
		VALUES ($1, $2, 'Event', 'event', 'published', now(), now() + interval '2 days',
			'Asia/Jakarta', 'Banda Aceh', 'Street', 'Terms') RETURNING id`, org, organizer)
	slot := insertID(t, ctx, s, `INSERT INTO event_slots (event_id, code, starts_at, ends_at)
		SELECT id, 'whole-event', starts_at, ends_at FROM events WHERE id = $1 RETURNING id`, event)
	execBookingSQL(t, ctx, s, `INSERT INTO event_pricing_versions (event_id, revision, created_by, rules, effective_at)
		VALUES ($1, 1, $2, '{"default_base_price":30000}', now())`, event, organizer)
	createTestStands(t, ctx, s, event, organizer)
	source := testOffers{Offer{PriceRevision: 1, Rent: 30_000, PlatformFee: 1_000, ProviderFee: 500,
		PolicyVersion: "test-policy", RefundPolicy: json.RawMessage(`{"test": true}`)}}
	lifetime := 24 * time.Hour
	status := func(id string) string {
		t.Helper()
		var v string
		if err := s.DB.QueryRow(ctx, `SELECT status FROM applications WHERE id = $1`, id).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	allocations := func() int {
		var n int
		if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM allocations`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Direct events refuse applications; curated events refuse direct holds.
	if _, err := s.Submit(ctx, tenantA, event, ApplicationInput{BusinessID: businessA}); !errors.Is(err, ErrNotCurated) {
		t.Fatalf("direct event application: %v", err)
	}
	if err := s.SetRegistrationMode(ctx, stranger, event, "curated"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stranger changed mode: %v", err)
	}
	if err := s.SetRegistrationMode(ctx, organizer, event, "curated"); err != nil {
		t.Fatal(err)
	}
	if mode, err := s.RegistrationMode(ctx, tenantA, event); err != nil || mode != "curated" {
		t.Fatalf("registration mode: %q %v", mode, err)
	}
	if _, err := s.RegistrationMode(ctx, tenantA, "bad"); !errors.Is(err, ErrApplicationInput) {
		t.Fatalf("invalid event id: %v", err)
	}
	direct := HoldInput{BusinessID: businessA, EventID: event, StandID: testStandOne, EventSlotID: slot, AcceptedPolicyVersion: "test-policy"}
	if _, err := s.CreateHold(ctx, tenantA, "direct", direct, HoldSettings{Lifetime: time.Minute, MaxActive: 1}, source); !errors.Is(err, ErrNotBookable) {
		t.Fatalf("direct hold on curated event: %v", err)
	}

	// Submission checks ownership and never claims inventory.
	if _, err := s.Submit(ctx, tenantB, event, ApplicationInput{BusinessID: businessA}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("applying for another tenant's business: %v", err)
	}
	if _, err := s.Submit(ctx, tenantA, event, ApplicationInput{BusinessID: businessA, PreferredStandIDs: []string{"bad"}}); !errors.Is(err, ErrApplicationInput) {
		t.Fatalf("invalid stand id: %v", err)
	}
	appA, err := s.Submit(ctx, tenantA, event, ApplicationInput{BusinessID: businessA, PreferredStandIDs: []string{testStandOne}})
	if err != nil || appA.Status != "submitted" || allocations() != 0 {
		t.Fatalf("submit: %+v %v allocations=%d", appA, err, allocations())
	}
	if _, err := s.Submit(ctx, tenantA, event, ApplicationInput{BusinessID: businessA}); !errors.Is(err, ErrApplicationDup) {
		t.Fatalf("duplicate: %v", err)
	}
	appB, err := s.Submit(ctx, tenantB, event, ApplicationInput{BusinessID: businessB})
	if err != nil {
		t.Fatal(err)
	}

	// Only the organizer sees and decides applications.
	if _, err := s.ListForEvent(ctx, stranger, event, 10); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stranger list: %v", err)
	}
	if list, err := s.ListForEvent(ctx, organizer, event, 10); err != nil || len(list) != 2 {
		t.Fatalf("organizer list: %v %v", list, err)
	}
	if _, err := s.ListForBusiness(ctx, tenantB, businessA, 10); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cross-tenant list: %v", err)
	}
	if _, err := s.Approve(ctx, stranger, appA.ID, ApproveInput{StandID: testStandOne, EventSlotID: slot}, lifetime, 1, source); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stranger approve: %v", err)
	}
	if _, err := s.Reject(ctx, tenantA, appA.ID, "no"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("applicant rejecting own application: %v", err)
	}
	if _, err := s.Reject(ctx, organizer, appB.ID, "  "); !errors.Is(err, ErrApplicationInput) {
		t.Fatalf("reject without reason: %v", err)
	}

	// Two approvals for one stand: exactly one wins, the loser stays submitted.
	type outcome struct {
		id  string
		err error
	}
	results := make(chan outcome, 2)
	start := make(chan struct{})
	for _, id := range []string{appA.ID, appB.ID} {
		go func() {
			<-start
			_, err := s.Approve(ctx, organizer, id, ApproveInput{StandID: testStandOne, EventSlotID: slot}, lifetime, 1, source)
			results <- outcome{id, err}
		}()
	}
	close(start)
	first, second := <-results, <-results
	winner, loser := first, second
	if winner.err != nil {
		winner, loser = second, first
	}
	if winner.err != nil || !errors.Is(loser.err, ErrStandUnavailable) {
		t.Fatalf("want one winner and a stand conflict; got %v and %v", winner.err, loser.err)
	}
	if status(winner.id) != "approved" || status(loser.id) != "submitted" || allocations() != 1 {
		t.Fatalf("states %s/%s allocations=%d", status(winner.id), status(loser.id), allocations())
	}
	if _, err := s.Approve(ctx, organizer, winner.id, ApproveInput{StandID: testStandTwo, EventSlotID: slot}, lifetime, 1, source); !errors.Is(err, ErrApplicationState) {
		t.Fatalf("second approval: %v", err)
	}

	// The offer is a normal booking owned by the applicant, with an offer allocation and a fixed price.
	var bookingID, bookingStatus, allocation string
	var total int64
	var owner string
	if err := s.DB.QueryRow(ctx, `SELECT a.booking_id, b.status, b.total, b.created_by, al.status FROM applications a
		JOIN bookings b ON b.id = a.booking_id JOIN allocations al ON al.booking_id = b.id WHERE a.id = $1`, winner.id).
		Scan(&bookingID, &bookingStatus, &total, &owner, &allocation); err != nil {
		t.Fatal(err)
	}
	winnerTenant := tenantA
	winnerBusiness := businessA
	if winner.id == appB.ID {
		winnerTenant, winnerBusiness = tenantB, businessB
	}
	if bookingStatus != "held" || total != 31_500 || allocation != "offer" || owner != winnerTenant {
		t.Fatalf("offer booking: %s %d %s %s", bookingStatus, total, allocation, owner)
	}
	if view, err := s.detail(ctx, winnerTenant, bookingID); err != nil || view.ExpiresAt.Before(time.Now().Add(23*time.Hour)) {
		t.Fatalf("applicant cannot see the offer or its lifetime is wrong: %+v %v", view, err)
	}
	if list, err := s.ListForBusiness(ctx, winnerTenant, winnerBusiness, 10); err != nil || len(list) != 1 ||
		list[0].Status != "approved" || list[0].OfferExpiresAt == nil {
		t.Fatalf("tenant view: %+v %v", list, err)
	}

	// One active hold per account per event: a second business of the same owner cannot be approved yet.
	secondBusiness := insertID(t, ctx, s, `INSERT INTO businesses (owner_id, category_id, name) VALUES ($1, $2, 'Second') RETURNING id`, winnerTenant, category)
	execBookingSQL(t, ctx, s, `INSERT INTO memberships (user_id, business_id, role) VALUES ($1, $2, 'owner')`, winnerTenant, secondBusiness)
	extra, err := s.Submit(ctx, winnerTenant, event, ApplicationInput{BusinessID: secondBusiness})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Approve(ctx, organizer, extra.ID, ApproveInput{StandID: testStandTwo, EventSlotID: slot}, lifetime, 1, source); !errors.Is(err, ErrApplicantBusy) {
		t.Fatalf("second active offer for one account: %v", err)
	}
	if status(extra.ID) != "submitted" || allocations() != 1 {
		t.Fatalf("busy applicant changed state: %s allocations=%d", status(extra.ID), allocations())
	}
	if _, err := s.Reject(ctx, organizer, extra.ID, "Menunggu offer pertama"); err != nil {
		t.Fatal(err)
	}

	// Rejection records the reason and cannot be repeated.
	rejected, err := s.Reject(ctx, organizer, loser.id, "Kategori penuh")
	if err != nil || rejected.Status != "rejected" || rejected.Reason == nil {
		t.Fatalf("reject: %+v %v", rejected, err)
	}
	if _, err := s.Reject(ctx, organizer, loser.id, "again"); !errors.Is(err, ErrApplicationState) {
		t.Fatalf("repeat reject: %v", err)
	}

	// Mode cannot flip while applications or bookings are live.
	if err := s.SetRegistrationMode(ctx, organizer, event, "direct"); !errors.Is(err, ErrApplicationState) {
		t.Fatalf("mode flip with live offers: %v", err)
	}

	// A lapsed offer releases the stand, reads as expired, and lets the business apply again.
	execBookingSQL(t, ctx, s, `UPDATE bookings SET expires_at = clock_timestamp() - interval '1 second' WHERE id = $1`, bookingID)
	if list, err := s.ListForBusiness(ctx, winnerTenant, winnerBusiness, 10); err != nil || list[0].Status != "expired" {
		t.Fatalf("lapsed offer view: %+v %v", list, err)
	}
	if n, err := s.Expire(ctx, 10); err != nil || n != 1 {
		t.Fatalf("expire: %d %v", n, err)
	}
	again, err := s.Submit(ctx, winnerTenant, event, ApplicationInput{BusinessID: winnerBusiness})
	if err != nil || again.ID == winner.id || status(winner.id) != "expired" {
		t.Fatalf("reapply: %+v %v status=%s", again, err, status(winner.id))
	}
	if _, err := s.Approve(ctx, organizer, again.ID, ApproveInput{StandID: testStandOne, EventSlotID: slot}, lifetime, 1, source); err != nil {
		t.Fatalf("stand should be offerable again: %v", err)
	}
}
