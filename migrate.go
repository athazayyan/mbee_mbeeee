package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
)

//go:embed migrations/00001_auth.up.sql
var authMigration string

//go:embed migrations/00002_auth_tokens.up.sql
var authTokensMigration string

// Applied in order. Each runs once; applied names are kept in schema_migrations.
var migrations = []struct{ name, sql string }{
	{"00001_auth.up.sql", authMigration},
	{"00002_auth_tokens.up.sql", authTokensMigration},
}

func migrate(ctx context.Context) error {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return errors.New("DATABASE_URL is required to run the migration")
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("connect to PostgreSQL: %w", err)
	}
	defer conn.Close(context.Background())

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		name text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("prepare schema_migrations: %w", err)
	}
	// Databases migrated before this table existed already contain 00001.
	if _, err := conn.Exec(ctx, `INSERT INTO schema_migrations (name)
		SELECT '00001_auth.up.sql' WHERE to_regclass('public.users') IS NOT NULL
		ON CONFLICT DO NOTHING`); err != nil {
		return fmt.Errorf("record existing migration: %w", err)
	}

	for _, m := range migrations {
		var applied bool
		if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE name = $1)", m.name).Scan(&applied); err != nil {
			return err
		}
		if applied {
			log.Printf("Skipped %s (already applied)", m.name)
			continue
		}
		if _, err := conn.Exec(ctx, m.sql); err != nil {
			return fmt.Errorf("apply %s: %w", m.name, err)
		}
		if _, err := conn.Exec(ctx, "INSERT INTO schema_migrations (name) VALUES ($1)", m.name); err != nil {
			return err
		}
		log.Printf("Applied %s", m.name)
	}
	return nil
}
