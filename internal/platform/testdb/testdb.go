// Package testdb menyiapkan PostgreSQL uji: skema sementara dengan semua migration, dan fixture booking minimal.
// Uji yang memakainya dilewati bila TENANTKU_TEST_DATABASE_URL kosong.
package testdb

import (
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Migrated membuat skema baru, menjalankan semua migration di dalamnya, dan menghapusnya setelah uji.
func Migrated(t testing.TB, ctx context.Context, prefix string) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TENANTKU_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TENANTKU_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	schema := pgx.Identifier{prefix + "_" + strings.ToLower(rand.Text())}.Sanitize()
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		defer conn.Close(cleanup)
		if _, err := conn.Exec(cleanup, "DROP SCHEMA IF EXISTS "+schema+" CASCADE"); err != nil {
			t.Error(err)
		}
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
	_, file, _, _ := runtime.Caller(0)
	directory := filepath.Join(filepath.Dir(file), "..", "..", "..", "migrations")
	files, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range files {
		// Pembatalan acara memakai fungsi berhak khusus yang terikat ke skema public.
		if !strings.HasSuffix(entry.Name(), ".up.sql") || entry.Name() == "00013_cancel_unpaid_event_bookings.up.sql" {
			continue
		}
		sql, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("%s: %v", entry.Name(), err)
		}
	}
	return pool
}

func Scalar(t testing.TB, ctx context.Context, pool *pgxpool.Pool, query string, args ...any) string {
	t.Helper()
	var value string
	if err := pool.QueryRow(ctx, query, args...).Scan(&value); err != nil {
		t.Fatalf("%v: %s", err, query)
	}
	return value
}

func Exec(t testing.TB, ctx context.Context, pool *pgxpool.Pool, query string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(ctx, query, args...); err != nil {
		t.Fatalf("%v: %s", err, query)
	}
}

// World adalah fixture minimal: tenant pemilik usaha, penyelenggara pemilik organisasi, satu acara, satu booking.
type World struct {
	Tenant, Business, Organizer, Organization, Event, Booking string
}

// User membuat akun terverifikasi dengan email unik.
func User(t testing.TB, ctx context.Context, pool *pgxpool.Pool, label string) string {
	t.Helper()
	return Scalar(t, ctx, pool, `INSERT INTO users (email, password_hash, email_verified_at) VALUES ($1, 'fixture', now()) RETURNING id`,
		label+"-"+strings.ToLower(rand.Text())+"@example.test")
}

// NewWorld membuat fixture. ended=true membuat acara berstatus completed yang sudah berakhir.
func NewWorld(t testing.TB, ctx context.Context, pool *pgxpool.Pool, bookingStatus string, ended bool) World {
	t.Helper()
	var w World
	w.Tenant = User(t, ctx, pool, "tenant")
	w.Organizer = User(t, ctx, pool, "organizer")
	category := Scalar(t, ctx, pool, `SELECT id FROM business_categories ORDER BY id LIMIT 1`)
	w.Business = Scalar(t, ctx, pool, `INSERT INTO businesses (owner_id, category_id, name) VALUES ($1, $2, 'Kopi Senja') RETURNING id`, w.Tenant, category)
	Exec(t, ctx, pool, `INSERT INTO memberships (user_id, business_id, role) VALUES ($1, $2, 'owner')`, w.Tenant, w.Business)
	w.Organization = Scalar(t, ctx, pool, `INSERT INTO organizations (owner_id, name) VALUES ($1, 'Pasar Malam') RETURNING id`, w.Organizer)
	Exec(t, ctx, pool, `INSERT INTO memberships (user_id, organization_id, role) VALUES ($1, $2, 'owner')`, w.Organizer, w.Organization)
	status, start := "published", "now() + interval '2 days'"
	if ended {
		status, start = "completed", "now() - interval '3 days'"
	}
	w.Event = Scalar(t, ctx, pool, `INSERT INTO events (organization_id, created_by, name, slug, status, starts_at, ends_at, timezone, city, address)
		VALUES ($1, $2, 'Bazar Uji', $3, '`+status+`', `+start+`, `+start+` + interval '1 day', 'Asia/Jakarta', 'Banda Aceh', 'Jalan Uji')
		RETURNING id`, w.Organization, w.Organizer, "bazar-"+strings.ToLower(rand.Text()))
	Exec(t, ctx, pool, `INSERT INTO event_pricing_versions (event_id, revision, created_by, rules, effective_at)
		VALUES ($1, 1, $2, '{"default_base_price":30000}', now() - interval '10 days')`, w.Event, w.Organizer)
	w.Booking = Scalar(t, ctx, pool, `INSERT INTO bookings (business_id, event_id, category_id, created_by, status, expires_at, price_revision,
		rent, platform_fee, provider_fee, terms_snapshot, refund_policy_version, refund_policy_snapshot)
		VALUES ($1, $2, $3, $4, $5, now(), 1, 30000, 2000, 0, 'syarat uji', 'uji', '{}') RETURNING id`,
		w.Business, w.Event, category, w.Tenant, bookingStatus)
	return w
}
