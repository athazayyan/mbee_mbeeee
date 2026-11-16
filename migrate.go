package main

import (
	"context"
	"crypto/sha256"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

//go:embed migrations/*.up.sql
var migrations embed.FS

var migrationName = regexp.MustCompile(`^(\d{5})_[a-z][a-z0-9_]*\.up\.sql$`)

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

	applied, err := applyMigrations(ctx, conn)
	if err != nil {
		return err
	}
	for _, name := range applied {
		log.Print(name)
	}
	if len(applied) == 0 {
		log.Print("Database migrations are up to date")
	}
	return nil
}

func applyMigrations(ctx context.Context, conn *pgx.Conn) ([]string, error) {
	files, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		return nil, err
	}

	var applied []string
	err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		// Serialize migration runners before inspecting or changing the schema.
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(1885825110)"); err != nil {
			return err
		}
		legacyDates, err := replaceNamedMigrationLedger(ctx, tx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			CREATE TABLE IF NOT EXISTS schema_migrations (
				version integer PRIMARY KEY,
				name text NOT NULL,
				checksum text NOT NULL,
				applied_at timestamptz NOT NULL DEFAULT now()
			)`); err != nil {
			return err
		}
		if err := relocateMigration(ctx, tx, 9, "00009_payments.up.sql", 16, "00016_payments.up.sql"); err != nil {
			return err
		}
		if err := relocateMigration(ctx, tx, 10, "00010_catalog.up.sql", 12, "00012_catalog.up.sql"); err != nil {
			return err
		}

		previous := 0
		for _, file := range files {
			name := file.Name()
			match := migrationName.FindStringSubmatch(name)
			if match == nil {
				return fmt.Errorf("invalid migration filename %s", name)
			}
			version, err := strconv.Atoi(match[1])
			if err != nil || version <= previous {
				return fmt.Errorf("invalid or duplicate migration version in %s", name)
			}
			previous = version
			content, err := migrations.ReadFile("migrations/" + name)
			if err != nil {
				return err
			}
			checksum := fmt.Sprintf("%x", sha256.Sum256(content))
			var recordedName, recordedHash string
			err = tx.QueryRow(ctx, "SELECT name, checksum FROM schema_migrations WHERE version = $1", version).Scan(&recordedName, &recordedHash)
			if err == nil {
				if recordedName != name || recordedHash != checksum {
					return fmt.Errorf("previously applied migration %05d differs from %s", version, name)
				}
				continue
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}

			sql := string(content)
			legacy := false
			if name == "00001_auth.up.sql" {
				legacy, err = legacyAuthExists(ctx, tx)
				if err != nil {
					return err
				}
				// The original migration was also run directly with psql and owns
				// transaction markers that the runner now supplies.
				sql = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(sql), "BEGIN;"), "COMMIT;")
			}
			if !legacy {
				if _, err := tx.Exec(ctx, sql); err != nil {
					return fmt.Errorf("apply %s: %w", name, err)
				}
			}
			if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version, name, checksum, applied_at)
				VALUES ($1, $2, $3, COALESCE($4, now()))`, version, name, checksum, legacyDates[name]); err != nil {
				return err
			}
			if legacy {
				applied = append(applied, "Recorded existing "+name)
			} else {
				applied = append(applied, "Applied "+name)
			}
		}
		return nil
	})
	return applied, err
}

// Relocate an exact migration that was already applied under a colliding version.
func relocateMigration(ctx context.Context, tx pgx.Tx, oldVersion int, oldName string, version int, name string) error {
	content, err := migrations.ReadFile("migrations/" + name)
	if err != nil {
		return err
	}
	checksum := fmt.Sprintf("%x", sha256.Sum256(content))
	_, err = tx.Exec(ctx, `UPDATE schema_migrations SET version = $1, name = $2
		WHERE version = $3 AND name = $4 AND checksum = $5`, version, name, oldVersion, oldName, checksum)
	return err
}

// The account-recovery branch used a name-only ledger before joining the versioned runner.
// Read its dates before replacing it in the same transaction as schema validation and migration.
func replaceNamedMigrationLedger(ctx context.Context, tx pgx.Tx) (map[string]*time.Time, error) {
	var legacy bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'schema_migrations' AND column_name = 'name'
	) AND NOT EXISTS (
		SELECT 1 FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'schema_migrations' AND column_name = 'version'
	)`).Scan(&legacy); err != nil || !legacy {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT name, applied_at FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	dates := make(map[string]*time.Time)
	for rows.Next() {
		var name string
		var appliedAt time.Time
		if err := rows.Scan(&name, &appliedAt); err != nil {
			rows.Close()
			return nil, err
		}
		switch name {
		case "00001_auth.up.sql":
		case "00002_auth_tokens.up.sql":
			name = "00008_auth_tokens.up.sql"
		default:
			rows.Close()
			return nil, fmt.Errorf("unknown migration in legacy ledger: %s", name)
		}
		dates[name] = &appliedAt
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `DROP TABLE schema_migrations`); err != nil {
		return nil, err
	}
	return dates, nil
}

func legacyAuthExists(ctx context.Context, tx pgx.Tx) (bool, error) {
	var count int
	err := tx.QueryRow(ctx, `
		SELECT count(*) FROM pg_class
		WHERE relnamespace = current_schema()::regnamespace AND relkind = 'r'
		AND relname IN ('users', 'sessions', 'session_refresh_tokens')
	`).Scan(&count)
	if err != nil || count == 0 {
		return false, err
	}
	if count != 3 {
		return false, errors.New("auth schema is incomplete; restore a consistent database before migrating")
	}

	var expectedColumns bool
	err = tx.QueryRow(ctx, `
		SELECT count(*) = 11 FROM information_schema.columns
		WHERE table_schema = current_schema() AND (table_name, column_name, udt_name) IN (
			('users', 'id', 'uuid'), ('users', 'email', 'text'), ('users', 'password_hash', 'bytea'),
			('users', 'created_at', 'timestamptz'), ('sessions', 'id', 'uuid'), ('sessions', 'user_id', 'uuid'),
			('sessions', 'expires_at', 'timestamptz'), ('sessions', 'revoked_at', 'timestamptz'),
			('session_refresh_tokens', 'token_hash', 'bytea'), ('session_refresh_tokens', 'session_id', 'uuid'),
			('session_refresh_tokens', 'used_at', 'timestamptz')
		)
	`).Scan(&expectedColumns)
	if err != nil {
		return false, err
	}
	if !expectedColumns {
		return false, errors.New("existing auth schema differs from migration 00001")
	}
	return true, nil
}
