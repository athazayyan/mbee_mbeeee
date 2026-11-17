package auth

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestSignupNormalize(t *testing.T) {
	good := Signup{Role: "tenant", Phone: "0812-3456 7890", BusinessName: "  Kopi   Senja ", CategoryID: "6F1A2B3C-0000-4000-8000-000000000001"}
	if fields := good.normalize(); len(fields) != 0 {
		t.Fatalf("valid tenant rejected: %v", fields)
	}
	if good.Phone != "+6281234567890" || good.BusinessName != "Kopi Senja" || good.CategoryID != strings.ToLower(good.CategoryID) {
		t.Fatalf("not normalized: %+v", good)
	}
	for _, phone := range []string{"+62 812 3456 7890", "6281234567890", "081234567890"} {
		if fields := (&Signup{Role: "organizer", Phone: phone, OrganizationName: "EO", EventName: "Pasar"}).normalize(); len(fields) != 0 {
			t.Errorf("phone %q rejected: %v", phone, fields)
		}
	}
	for name, input := range map[string]Signup{
		"landline":     {Role: "organizer", Phone: "0211234567", OrganizationName: "EO", EventName: "Pasar"},
		"letters":      {Role: "organizer", Phone: "08abc", OrganizationName: "EO", EventName: "Pasar"},
		"no role":      {Phone: "081234567890"},
		"no business":  {Role: "tenant", Phone: "081234567890", CategoryID: good.CategoryID},
		"no category":  {Role: "tenant", Phone: "081234567890", BusinessName: "Kopi"},
		"no event":     {Role: "organizer", Phone: "081234567890", OrganizationName: "EO"},
		"no organizer": {Role: "organizer", Phone: "081234567890", EventName: "Pasar"},
	} {
		if len(input.normalize()) == 0 {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestRegisterWithProfile(t *testing.T) {
	databaseURL := os.Getenv("TENANTKU_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set TENANTKU_TEST_DATABASE_URL to test against a migrated PostgreSQL database")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	service, err := New(ctx, pool, rand.Text()+rand.Text(), rand.Text()+rand.Text())
	if err != nil {
		t.Fatal(err)
	}
	var category string
	if err := pool.QueryRow(ctx, `SELECT id FROM business_categories LIMIT 1`).Scan(&category); err != nil {
		t.Skipf("no business categories seeded: %v", err)
	}
	cleanup := func(id string) {
		t.Cleanup(func() {
			c, stop := context.WithTimeout(context.Background(), 3*time.Second)
			defer stop()
			for _, query := range []string{
				`DELETE FROM businesses WHERE owner_id = $1`, `DELETE FROM organizations WHERE owner_id = $1`,
				`DELETE FROM users WHERE id = $1`,
			} {
				if _, err := pool.Exec(c, query, id); err != nil {
					t.Error(err)
				}
			}
		})
	}
	const password = "signup fixture password"
	address := func() string { return strings.ToLower(rand.Text()) + "@example.com" }

	tenant := Signup{Role: "tenant", Phone: "081234567890", BusinessName: "Kopi Senja", CategoryID: category}
	if fields := tenant.normalize(); len(fields) != 0 {
		t.Fatal(fields)
	}
	account, err := service.register(ctx, address(), password, &tenant)
	if err != nil {
		t.Fatal(err)
	}
	cleanup(account.ID)
	var kind, phone, name string
	if err := pool.QueryRow(ctx, `SELECT s.role, s.phone, b.name FROM signup_profiles s
		JOIN memberships m ON m.user_id = s.user_id AND m.role = 'owner' JOIN businesses b ON b.id = m.business_id
		WHERE s.user_id = $1`, account.ID).Scan(&kind, &phone, &name); err != nil {
		t.Fatal(err)
	}
	if kind != "tenant" || phone != "+6281234567890" || name != "Kopi Senja" {
		t.Fatalf("tenant profile: %s %s %s", kind, phone, name)
	}

	organizer := Signup{Role: "organizer", Phone: "+62 811 222 3333", OrganizationName: "EO Nusantara", EventName: "Pasar Malam"}
	organizer.normalize()
	other, err := service.register(ctx, address(), password, &organizer)
	if err != nil {
		t.Fatal(err)
	}
	cleanup(other.ID)
	var event string
	if err := pool.QueryRow(ctx, `SELECT o.name, s.event_name FROM signup_profiles s
		JOIN memberships m ON m.user_id = s.user_id JOIN organizations o ON o.id = m.organization_id
		WHERE s.user_id = $1`, other.ID).Scan(&name, &event); err != nil {
		t.Fatal(err)
	}
	if name != "EO Nusantara" || event != "Pasar Malam" {
		t.Fatalf("organizer profile: %s %s", name, event)
	}

	// An unknown category fails the whole registration, leaving no half-created account.
	bad := Signup{Role: "tenant", Phone: "081234567890", BusinessName: "Kopi", CategoryID: "00000000-0000-4000-8000-000000000000"}
	bad.normalize()
	email := address()
	if _, err := service.register(ctx, email, password, &bad); !errors.Is(err, errBadCategory) {
		t.Fatalf("unknown category: %v", err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE email = $1`, email).Scan(&count); err != nil || count != 0 {
		t.Fatalf("account left behind: %d %v", count, err)
	}
}
