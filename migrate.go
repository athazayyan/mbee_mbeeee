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
		// Migration branch khalisha pernah memakai nomor 11–12 lalu 17–25. Pindahkan ke 27–35 sebelum
		// katalog menempati 12 dan migration main (aplikasi, paket, profil pendaftaran) menempati 17–26.
		if err := relocateMigration(ctx, tx, 12, "00012_pos_sales.up.sql", 28, "00028_pos_sales.up.sql"); err != nil {
			return err
		}
		if err := relocateMigration(ctx, tx, 11, "00011_products.up.sql", 27, "00027_products.up.sql"); err != nil {
			return err
		}
		if err := relocateMigration(ctx, tx, 17, "00017_products.up.sql", 27, "00027_products.up.sql"); err != nil {
			return err
		}
		if err := relocateMigration(ctx, tx, 18, "00018_pos_sales.up.sql", 28, "00028_pos_sales.up.sql"); err != nil {
			return err
		}
		if err := relocateMigration(ctx, tx, 19, "00019_admin_plans.up.sql", 29, "00029_admin_plans.up.sql"); err != nil {
			return err
		}
		if err := relocateMigration(ctx, tx, 20, "00020_sale_adjustments_expenses.up.sql", 30, "00030_sale_adjustments_expenses.up.sql"); err != nil {
			return err
		}
		if err := relocateMigration(ctx, tx, 21, "00021_pos_devices.up.sql", 31, "00031_pos_devices.up.sql"); err != nil {
			return err
		}
		if err := relocateMigration(ctx, tx, 22, "00022_business_reports.up.sql", 32, "00032_business_reports.up.sql"); err != nil {
			return err
		}
		if err := relocateMigration(ctx, tx, 23, "00023_notifications.up.sql", 33, "00033_notifications.up.sql"); err != nil {
			return err
		}
		if err := relocateMigration(ctx, tx, 24, "00024_conversations.up.sql", 34, "00034_conversations.up.sql"); err != nil {
			return err
		}
		if err := relocateMigration(ctx, tx, 25, "00025_reviews.up.sql", 35, "00035_reviews.up.sql"); err != nil {
			return err
		}
		// Unggahan sempat memakai 36 sebelum main menempatinya dengan rincian biaya pembayaran.
		if err := relocateMigration(ctx, tx, 36, "00036_uploads.up.sql", 37, "00037_uploads.up.sql"); err != nil {
			return err
		}
		if err := relocateMigration(ctx, tx, 10, "00010_catalog.up.sql", 12, "00012_catalog.up.sql"); err != nil {
			return err
		}
		// Dana (check-in, sengketa, pencairan) sempat memakai 37 lalu 38 sebelum main menempati 38–42.
		if err := relocateMigration(ctx, tx, 38, "00038_funds.up.sql", 44, "00044_funds.up.sql"); err != nil {
			return err
		}
		aidilAdmin, err := reconcileAidilMigrations(ctx, tx)
		if err != nil {
			return err
		}
		var existingBreakdown bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM schema_migrations WHERE version = 25 AND name = '00025_booking_payment_breakdown.up.sql'
		)`).Scan(&existingBreakdown); err != nil {
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

			sql := mergeMigrationSQL(name, string(content), existingBreakdown, aidilAdmin)
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

func reconcileAidilMigrations(ctx context.Context, tx pgx.Tx) (bool, error) {
	for _, move := range []struct {
		from, to int
		name     string
	}{
		{27, 38, "event_cancellation_refunds"},
		{28, 39, "event_refund_processing"},
		{29, 40, "cancelled_payment_reconciliation"},
		{30, 41, "manual_event_refund_receipts"},
	} {
		oldName := fmt.Sprintf("%05d_%s.up.sql", move.from, move.name)
		name := fmt.Sprintf("%05d_%s.up.sql", move.to, move.name)
		if err := relocateMigration(ctx, tx, move.from, oldName, move.to, name); err != nil {
			return false, err
		}
	}
	// The old admin migration created the same audit tables as the beginning of 00029.
	// Keep the original ledger entry under a free version and run the remaining plan DDL.
	const adminChecksum = "f3d1c20c6c73e29b099b9b8881a43615211ca9738dac2e2858efdb86595fbaff"
	if _, err := tx.Exec(ctx, `UPDATE schema_migrations SET version = 43
		WHERE version = 31 AND name = '00031_platform_admin_audit.up.sql' AND checksum = $1`, adminChecksum); err != nil {
		return false, err
	}
	var adminExists bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM schema_migrations
		WHERE version = 43 AND name = '00031_platform_admin_audit.up.sql' AND checksum = $1
	)`, adminChecksum).Scan(&adminExists)
	return adminExists, err
}

var (
	ledgerAccountCheck = regexp.MustCompile(`ledger_entries_account_check\s+CHECK \(account IN\s*\([^)]*\)\)`)
	ledgerKindCheck    = regexp.MustCompile(`ledger_transactions_kind_check\s+CHECK \(kind IN\s*\([^)]*\)\)`)
)

const (
	ledgerAccounts = "ledger_entries_account_check CHECK (account IN ('provider_clearing', 'organizer_escrow', " +
		"'platform_revenue', 'provider_fee_payable', 'manual_refund_cash', 'refund_payable', 'payout_payable'))"
	ledgerKinds = "ledger_transactions_kind_check CHECK (kind IN ('payment_received', 'plan_payment_received', " +
		"'reversal', 'event_cancellation_refund', 'refund_reserved', 'refund_issued', 'payout_reserved', 'payout_released'))"
)

// Keep published migration files and checksums intact while reconciling the two release histories.
func mergeMigrationSQL(name, sql string, existingBreakdown, aidilAdmin bool) string {
	switch name {
	case "00020_payment_methods.up.sql":
		if existingBreakdown {
			sql = strings.Replace(sql,
				"base_amount = b.rent + b.platform_fee, method_fee = a.amount - b.rent - b.platform_fee",
				"base_amount = COALESCE(a.rent_amount + a.platform_fee_amount, b.rent + b.platform_fee), "+
					"method_fee = COALESCE(a.provider_fee_amount, a.amount - b.rent - b.platform_fee)", 1)
			sql = strings.Replace(sql, "'refund_issued', 'payout_released'", "'refund_issued', 'payout_released', 'event_cancellation_refund'", 1)
		}
	case "00029_admin_plans.up.sql":
		if aidilAdmin {
			_, remaining, _ := strings.Cut(sql, "ALTER TABLE organizations ADD COLUMN")
			sql = "ALTER TABLE organizations ADD COLUMN" + remaining
		}
	case "00041_manual_event_refund_receipts.up.sql":
		// The ledger checks of 00041, 00042 and 00044 are patched to the same union that 00045 installs,
		// so rows written by either feature stay valid while the migrations run.
		sql = ledgerAccountCheck.ReplaceAllString(sql, ledgerAccounts)
	case "00042_merge_ledger_kinds.up.sql":
		sql = ledgerKindCheck.ReplaceAllString(sql, ledgerKinds)
	case "00044_funds.up.sql":
		sql = ledgerKindCheck.ReplaceAllString(ledgerAccountCheck.ReplaceAllString(sql, ledgerAccounts), ledgerKinds)
	case "00039_event_refund_processing.up.sql":
		// The combined ledger check is installed by 00042.
		sql, _, _ = strings.Cut(sql, "ALTER TABLE ledger_transactions")
	}
	return sql
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
