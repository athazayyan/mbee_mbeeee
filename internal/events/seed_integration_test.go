package events

import (
	"context"
	"errors"
	"testing"
	"time"

	"kitakarsa.com/tenantku/backend/internal/platform/testdb"
)

// The demo seed goes through the real create, layout, pricing, and publish rules, and an organizer then has one
// active event at a time: a new one is possible only after the current one has ended.
func TestSeedPublishedAndOneActiveEvent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool := testdb.Migrated(t, ctx, "seed")
	s := Store{DB: pool}
	owner := testdb.User(t, ctx, pool, "eo")
	org := testdb.Scalar(t, ctx, pool, `INSERT INTO organizations (owner_id, name) VALUES ($1, 'EO Demo') RETURNING id`, owner)
	testdb.Exec(t, ctx, pool, `INSERT INTO memberships (user_id, organization_id, role) VALUES ($1, $2, 'owner')`, owner, org)

	start := time.Now().Add(14 * 24 * time.Hour)
	in := SeedEvent{
		Name: "Pasar Uji", Slug: "pasar-uji", City: "Banda Aceh", Address: "Jl. Uji", Description: "Uji", Instagram: "pasar.uji",
		Terms: "Syarat uji", Facilities: []string{"Listrik"}, StartsAt: start, EndsAt: start.Add(48 * time.Hour),
		Blocks: []SeedBlock{{"Kuliner", 5, 300000}, {"Kriya", 3, 200000}},
	}
	id, stands, err := s.SeedPublished(ctx, owner, org, in)
	if err != nil || len(stands) != 8 {
		t.Fatalf("seed: %v (%d stands)", err, len(stands))
	}
	if testdb.Scalar(t, ctx, pool, `SELECT status || '/' || instagram FROM events WHERE id = $1`, id) != "published/pasar.uji" ||
		testdb.Scalar(t, ctx, pool, `SELECT count(*)::text FROM stands WHERE event_id = $1 AND active AND removed_at IS NULL`, id) != "8" {
		t.Fatal("the seeded event is not published with its stands")
	}

	second := eventInput{Name: ptr("Acara Kedua"), Slug: ptr("acara-kedua"), City: ptr("Lhokseumawe"), Address: ptr("Jl. Dua"),
		Timezone: ptr("Asia/Jakarta"), StartsAt: ptr(start.Add(72 * time.Hour)), EndsAt: ptr(start.Add(96 * time.Hour))}
	if _, err := s.create(ctx, owner, org, second); !errors.Is(err, errActiveEventExists) {
		t.Fatalf("second active event: %v", err)
	}
	// Cancelled or finished events no longer count.
	testdb.Exec(t, ctx, pool, `UPDATE events SET status = 'cancelled' WHERE id = $1`, id)
	if _, err := s.create(ctx, owner, org, second); err != nil {
		t.Fatalf("new event after cancelling: %v", err)
	}
	other := second
	other.Slug = ptr("acara-ketiga")
	if _, err := s.create(ctx, owner, org, other); !errors.Is(err, errActiveEventExists) {
		t.Fatalf("a draft is active too: %v", err)
	}
}

func TestInstagramInput(t *testing.T) {
	for in, want := range map[string]string{"@pasar.uji": "pasar.uji", " pasar_uji ": "pasar_uji", "": ""} {
		input := eventInput{Instagram: ptr(in)}
		if fields := validateEventInput(input, false, false); fields["instagram"] != "" || *input.Instagram != want {
			t.Errorf("%q: %v -> %q", in, fields, *input.Instagram)
		}
	}
	for _, bad := range []string{"dua kata", "a/b", "https://instagram.com/x", "terlalu.panjang.sekali.akunnya.12345"} {
		if fields := validateEventInput(eventInput{Instagram: ptr(bad)}, false, false); fields["instagram"] == "" {
			t.Errorf("%q accepted", bad)
		}
	}
}
