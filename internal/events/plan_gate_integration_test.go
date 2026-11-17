package events

import (
	"context"
	"crypto/rand"
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

func TestPublishRequiresActivePlan(t *testing.T) {
	url := os.Getenv("TENANTKU_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TENANTKU_TEST_DATABASE_URL to run the PostgreSQL plan gate test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	schema := pgx.Identifier{"plan_gate_" + strings.ToLower(rand.Text())}.Sanitize()
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

	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	var user, org, event string
	must := func(dst *string, query string, args ...any) {
		t.Helper()
		if err := pool.QueryRow(ctx, query, args...).Scan(dst); err != nil {
			t.Fatal(err)
		}
	}
	must(&user, `INSERT INTO users (email, password_hash) VALUES ('o@example.com', 'x') RETURNING id`)
	must(&org, `INSERT INTO organizations (owner_id, name) VALUES ($1, 'Org') RETURNING id`, user)
	exec(`INSERT INTO memberships (user_id, organization_id, role) VALUES ($1, $2, 'owner')`, user, org)
	must(&event, `INSERT INTO events (organization_id, created_by, name, slug, starts_at, ends_at, timezone, city, address, terms)
		VALUES ($1, $2, 'E', 'e', now(), now() + interval '2 days', 'Asia/Jakarta', 'Banda Aceh', 'St', 'T') RETURNING id`, org, user)
	exec(`INSERT INTO event_layouts (event_id, revision, width, height, cell_meters, updated_by) VALUES ($1, 1, 10, 10, 1, $2)`, event, user)
	exec(`INSERT INTO stands (id, event_id, code, x, y, width, height)
		VALUES (gen_random_uuid(), $1, 'A1', 1, 1, 1, 1), (gen_random_uuid(), $1, 'A2', 3, 1, 1, 1)`, event)

	check := func(s Store, in error) error {
		t.Helper()
		var out error
		err := pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
			out = s.withPlan(ctx, tx, event, in)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	var fields fieldErrors
	// Off by default: existing behaviour is untouched.
	if err := check(Store{}, nil); err != nil {
		t.Fatalf("gate off: %v", err)
	}
	// On, without a plan: publication is blocked and other problems are kept.
	if err := check(Store{RequirePlan: true}, fieldErrors{"terms": "x"}); !errors.As(err, &fields) || fields["plan"] == "" || fields["terms"] != "x" {
		t.Fatalf("gate on without plan: %v", err)
	}
	if err := check(Store{RequirePlan: true}, errors.New("database down")); err == nil || errors.As(err, &fields) {
		t.Fatalf("unrelated errors must pass through: %v", err)
	}
	// A pending purchase is not enough.
	exec(`INSERT INTO event_plan_purchases (event_id, plan_code, purchased_by, amount) VALUES ($1, 'standard', $2, 100000)`, event, user)
	if err := check(Store{RequirePlan: true}, nil); err == nil {
		t.Fatal("pending purchase must not satisfy the gate")
	}
	store := Store{DB: pool, RequirePlan: true}
	layout, err := store.organizerLayout(ctx, user, org, event)
	if err != nil {
		t.Fatal(err)
	}
	save := func() error {
		_, err := store.saveLayout(ctx, user, event, layoutETag(layout.Revision), layout.layoutInput)
		return err
	}
	if err := save(); !errors.As(err, &fields) || fields["plan"] == "" {
		t.Fatalf("pending plan must block layout changes: %v", err)
	}
	// Active plan with enough capacity passes; too little capacity does not.
	exec(`UPDATE event_plan_purchases SET status = 'active', activated_at = now(), capacity_stands = 2 WHERE event_id = $1`, event)
	if err := check(Store{RequirePlan: true}, nil); err != nil {
		t.Fatalf("active plan: %v", err)
	}
	exec(`UPDATE event_plan_purchases SET capacity_stands = 1 WHERE event_id = $1`, event)
	if err := check(Store{RequirePlan: true}, nil); !errors.As(err, &fields) || !strings.Contains(fields["plan"], "maksimal 1 stand") {
		t.Fatalf("capacity exceeded: %v", err)
	}
	if err := save(); !errors.As(err, &fields) || !strings.Contains(fields["plan"], "maksimal 1 stand") {
		t.Fatalf("capacity must block layout changes: %v", err)
	}
	layout.Stands[1].Active = false
	if err := save(); err != nil {
		t.Fatalf("layout within active plan capacity: %v", err)
	}
}
