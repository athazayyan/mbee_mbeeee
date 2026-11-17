package reports

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	DB *pgxpool.Pool
}

var errNotFound = errors.New("report not found")

func (s Store) Ready(ctx context.Context) error {
	_, err := s.DB.Exec(ctx, `SELECT 1 FROM business_reports, report_shares LIMIT 0`)
	return err
}

func requireOwner(ctx context.Context, tx pgx.Tx, actor, business string) error {
	var found bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM memberships
		WHERE user_id = $1 AND business_id = $2 AND role = 'owner')`, actor, business).Scan(&found); err != nil {
		return err
	}
	if !found {
		return errNotFound
	}
	return nil
}

const codeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// verificationCode membuat kode pendek seperti "K7QM-3XPA" yang dicetak di rapor.
func verificationCode() (string, error) {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	code := make([]byte, 0, 9)
	for index, value := range raw {
		if index == 4 {
			code = append(code, '-')
		}
		code = append(code, codeAlphabet[int(value)%len(codeAlphabet)])
	}
	return string(code), nil
}

type reportInput struct {
	PeriodStart time.Time `json:"period_start"`
	PeriodEnd   time.Time `json:"period_end"`
	EventID     *string   `json:"event_id,omitempty"`
	Timezone    string    `json:"timezone,omitempty"`
}

// build membaca penjualan, item, pengeluaran, dan perangkat untuk periode lalu menghitung snapshot.
func build(ctx context.Context, tx pgx.Tx, business string, input reportInput, cutoff time.Time) (Snapshot, error) {
	var snapshot Snapshot
	var name string
	if err := tx.QueryRow(ctx, `SELECT name FROM businesses WHERE id = $1`, business).Scan(&name); err != nil {
		return snapshot, err
	}
	var eventName *string
	if input.EventID != nil {
		var value string
		if err := tx.QueryRow(ctx, `SELECT name FROM events WHERE id = $1`, *input.EventID).Scan(&value); err != nil {
			return snapshot, err
		}
		eventName = &value
	}
	salesRows, err := tx.Query(ctx, `SELECT s.id::text, to_char(s.client_created_at AT TIME ZONE $5, 'YYYY-MM-DD'),
		s.total, s.discount, s.method,
		COALESCE((SELECT sum(a.amount) FROM sale_adjustments a WHERE a.sale_ref = s.id AND a.created_at <= $6), 0),
		EXISTS (SELECT 1 FROM sale_adjustments a WHERE a.sale_ref = s.id AND a.type = 'void' AND a.created_at <= $6),
		EXISTS (SELECT 1 FROM sale_adjustments a WHERE a.sale_ref = s.id AND a.type = 'return' AND a.created_at <= $6)
		FROM sales s WHERE s.business_id = $1 AND s.client_created_at >= $2 AND s.client_created_at < $3
		AND ($4::uuid IS NULL OR s.event_id = $4) AND s.received_at <= $6`,
		business, input.PeriodStart, input.PeriodEnd, input.EventID, input.Timezone, cutoff)
	if err != nil {
		return snapshot, err
	}
	sales, err := pgx.CollectRows(salesRows, pgx.RowToStructByPos[saleRow])
	if err != nil {
		return snapshot, err
	}
	ids := make([]string, len(sales))
	for index, sale := range sales {
		ids[index] = sale.ID
	}
	itemRows, err := tx.Query(ctx, `SELECT si.sale_ref::text, si.product_id::text, si.product_name, si.quantity, si.unit_price,
		si.unit_cost, si.discount, si.subtotal,
		COALESCE((SELECT sum(ai.quantity) FROM sale_adjustment_items ai JOIN sale_adjustments a ON a.id = ai.adjustment_id
			WHERE ai.sale_item_id = si.id AND a.created_at <= $2), 0)
		FROM sale_items si WHERE si.sale_ref = ANY($1::uuid[])`, ids, cutoff)
	if err != nil {
		return snapshot, err
	}
	items, err := pgx.CollectRows(itemRows, pgx.RowToStructByPos[itemRow])
	if err != nil {
		return snapshot, err
	}
	var expenses int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(amount), 0) FROM expenses WHERE business_id = $1
		AND occurred_at >= $2 AND occurred_at < $3 AND ($4::uuid IS NULL OR event_id = $4 OR event_id IS NULL)
		AND created_at <= $5`, business, input.PeriodStart, input.PeriodEnd, input.EventID, cutoff).Scan(&expenses); err != nil {
		return snapshot, err
	}
	snapshot = aggregate(sales, items, expenses)
	var completeness Completeness
	if err := tx.QueryRow(ctx, `SELECT count(DISTINCT s.device_id), max(d.last_synced_at)
		FROM sales s LEFT JOIN pos_devices d ON d.business_id = s.business_id AND d.id = s.device_id
		WHERE s.id = ANY($1::uuid[])`, ids).Scan(&completeness.Devices, &completeness.LastSyncedAt); err != nil {
		return snapshot, err
	}
	completeness.Note = fmt.Sprintf("Mencakup transaksi dari %d perangkat kasir yang tersinkron hingga %s. "+
		"Transaksi yang belum tersinkron atau dicatat di luar TenantKu tidak termasuk.",
		completeness.Devices, cutoff.Format("02-01-2006 15:04 MST"))
	snapshot.Completeness = completeness
	snapshot.BusinessName = name
	snapshot.EventName = eventName
	snapshot.PeriodStart, snapshot.PeriodEnd, snapshot.Timezone = input.PeriodStart, input.PeriodEnd, input.Timezone
	snapshot.GeneratedAt, snapshot.CutoffAt = cutoff, cutoff
	snapshot.Source = sourceLabel
	snapshot.VerificationStatus = "third_party_unverified"
	snapshot.VerificationLabel = verificationLabel
	return snapshot, nil
}

type Report struct {
	ID               string    `json:"id"`
	BusinessID       string    `json:"business_id"`
	EventID          *string   `json:"event_id,omitempty"`
	PeriodStart      time.Time `json:"period_start"`
	PeriodEnd        time.Time `json:"period_end"`
	Timezone         string    `json:"timezone"`
	Status           string    `json:"status"`
	VerificationCode string    `json:"verification_code"`
	CreatedAt        time.Time `json:"created_at"`
	NetSales         *int64    `json:"net_sales,omitempty"`
	Snapshot         *Snapshot `json:"snapshot,omitempty"`
}

// create membuat snapshot sekarang dan menyimpannya. Status sudah succeeded saat dikembalikan;
// kolom status disiapkan bila pembuatan dipindah ke worker untuk periode besar.
func (s Store) create(ctx context.Context, actor, business string, input reportInput) (Report, error) {
	var report Report
	err := pgx.BeginTxFunc(ctx, s.DB, pgx.TxOptions{IsoLevel: pgx.RepeatableRead}, func(tx pgx.Tx) error {
		if err := requireOwner(ctx, tx, actor, business); err != nil {
			return err
		}
		var cutoff time.Time
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&cutoff); err != nil {
			return err
		}
		snapshot, err := build(ctx, tx, business, input, cutoff)
		if err != nil {
			return err
		}
		encoded, err := json.Marshal(snapshot)
		if err != nil {
			return err
		}
		code, err := verificationCode()
		if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO business_reports (business_id, event_id, period_start, period_end, timezone,
			status, snapshot, verification_code, cutoff_at, requested_by, completed_at)
			VALUES ($1, $2, $3, $4, $5, 'succeeded', $6, $7, $8, $9, clock_timestamp())
			RETURNING id::text, created_at`, business, input.EventID, input.PeriodStart, input.PeriodEnd, input.Timezone,
			encoded, code, cutoff, actor).Scan(&report.ID, &report.CreatedAt); err != nil {
			return err
		}
		report.BusinessID, report.EventID = business, input.EventID
		report.PeriodStart, report.PeriodEnd, report.Timezone = input.PeriodStart, input.PeriodEnd, input.Timezone
		report.Status, report.VerificationCode, report.Snapshot = "succeeded", code, &snapshot
		report.NetSales = &snapshot.NetSales
		return nil
	})
	return report, err
}

const reportColumns = `r.id::text, r.business_id::text, r.event_id::text, r.period_start, r.period_end, r.timezone, r.status,
	r.verification_code, r.created_at, (r.snapshot->>'net_sales')::bigint`

func scanReport(row pgx.Row, withSnapshot bool) (Report, error) {
	var r Report
	var raw []byte
	targets := []any{&r.ID, &r.BusinessID, &r.EventID, &r.PeriodStart, &r.PeriodEnd, &r.Timezone, &r.Status,
		&r.VerificationCode, &r.CreatedAt, &r.NetSales}
	if withSnapshot {
		targets = append(targets, &raw)
	}
	if err := row.Scan(targets...); err != nil {
		return r, err
	}
	if withSnapshot && raw != nil {
		var snapshot Snapshot
		if err := json.Unmarshal(raw, &snapshot); err != nil {
			return r, err
		}
		r.Snapshot = &snapshot
	}
	return r, nil
}

func (s Store) list(ctx context.Context, actor, business string) ([]Report, error) {
	var result []Report
	err := pgx.BeginTxFunc(ctx, s.DB, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if err := requireOwner(ctx, tx, actor, business); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+reportColumns+` FROM business_reports r WHERE r.business_id = $1
			ORDER BY r.created_at DESC, r.id DESC LIMIT 50`, business)
		if err != nil {
			return err
		}
		result, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Report, error) { return scanReport(row, false) })
		return err
	})
	return result, err
}

// get mengembalikan rapor milik usaha yang pemiliknya adalah actor; rapor orang lain dianggap tidak ada.
func (s Store) get(ctx context.Context, actor, id string) (Report, error) {
	report, err := scanReport(s.DB.QueryRow(ctx, `SELECT `+reportColumns+`, r.snapshot FROM business_reports r
		JOIN memberships m ON m.business_id = r.business_id AND m.user_id = $1 AND m.role = 'owner'
		WHERE r.id = $2`, actor, id), true)
	if errors.Is(err, pgx.ErrNoRows) {
		return report, errNotFound
	}
	return report, err
}

type Share struct {
	ID           string     `json:"id"`
	URL          string     `json:"url,omitempty"`
	Scope        string     `json:"scope"`
	ExpiresAt    time.Time  `json:"expires_at"`
	RevokedAt    *time.Time `json:"revoked_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	ViewCount    int64      `json:"view_count"`
	LastViewedAt *time.Time `json:"last_viewed_at,omitempty"`
	Active       bool       `json:"active"`
}

func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// createShare membuat token acak 32 byte. Token hanya dikembalikan sekali di URL; yang disimpan hanya hash-nya.
func (s Store) createShare(ctx context.Context, actor, reportID string, expires time.Time, origin string) (Share, error) {
	report, err := s.get(ctx, actor, reportID)
	if err != nil {
		return Share{}, err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return Share{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	var share Share
	if err := s.DB.QueryRow(ctx, `INSERT INTO report_shares (business_id, report_id, token_hash, expires_at, created_by)
		VALUES ($1, $2, $3, $4, $5) RETURNING id::text, scope, expires_at, created_at`,
		report.BusinessID, report.ID, hashToken(token), expires, actor).
		Scan(&share.ID, &share.Scope, &share.ExpiresAt, &share.CreatedAt); err != nil {
		return Share{}, err
	}
	share.URL = origin + "/bagikan/rapor/" + token
	share.Active = true
	return share, nil
}

func (s Store) shares(ctx context.Context, actor, reportID string) ([]Share, error) {
	if _, err := s.get(ctx, actor, reportID); err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `SELECT id::text, scope, expires_at, revoked_at, created_at, view_count, last_viewed_at,
		revoked_at IS NULL AND expires_at > now() FROM report_shares WHERE report_id = $1 ORDER BY created_at DESC LIMIT 50`, reportID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Share, error) {
		var share Share
		err := row.Scan(&share.ID, &share.Scope, &share.ExpiresAt, &share.RevokedAt, &share.CreatedAt, &share.ViewCount,
			&share.LastViewedAt, &share.Active)
		return share, err
	})
}

func (s Store) revokeShare(ctx context.Context, actor, shareID string) error {
	tag, err := s.DB.Exec(ctx, `UPDATE report_shares rs SET revoked_at = COALESCE(rs.revoked_at, clock_timestamp())
		FROM memberships m WHERE rs.id = $2 AND m.business_id = rs.business_id AND m.user_id = $1 AND m.role = 'owner'`,
		actor, shareID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errNotFound
	}
	return nil
}

// PublicReport hanya memuat ringkasan untuk pemegang tautan (scope summary).
type PublicReport struct {
	BusinessName       string       `json:"business_name"`
	EventName          *string      `json:"event_name,omitempty"`
	PeriodStart        time.Time    `json:"period_start"`
	PeriodEnd          time.Time    `json:"period_end"`
	Timezone           string       `json:"timezone"`
	GeneratedAt        time.Time    `json:"generated_at"`
	VerificationStatus string       `json:"verification_status"`
	VerificationLabel  string       `json:"verification_label"`
	VerificationCode   string       `json:"verification_code"`
	Source             string       `json:"source"`
	NetSales           int64        `json:"net_sales"`
	TransactionCount   int64        `json:"transaction_count"`
	UnitsNet           int64        `json:"units_net"`
	AverageTransaction *int64       `json:"average_transaction,omitempty"`
	Daily              []DailyTotal `json:"daily"`
	CompletenessNote   string       `json:"completeness_note"`
	ExpiresAt          time.Time    `json:"expires_at"`
}

// public membaca rapor lewat token. Token tidak dikenal, kedaluwarsa, atau dicabut sama-sama dianggap tidak ada.
func (s Store) public(ctx context.Context, token string) (PublicReport, error) {
	var result PublicReport
	var raw []byte
	var shareID string
	err := s.DB.QueryRow(ctx, `SELECT rs.id::text, rs.expires_at, r.verification_code, r.snapshot FROM report_shares rs
		JOIN business_reports r ON r.id = rs.report_id
		WHERE rs.token_hash = $1 AND rs.revoked_at IS NULL AND rs.expires_at > now() AND r.status = 'succeeded'`,
		hashToken(token)).Scan(&shareID, &result.ExpiresAt, &result.VerificationCode, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, errNotFound
	}
	if err != nil {
		return result, err
	}
	var snapshot Snapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return result, err
	}
	if _, err := s.DB.Exec(ctx, `UPDATE report_shares SET view_count = view_count + 1, last_viewed_at = clock_timestamp()
		WHERE id = $1`, shareID); err != nil {
		return result, err
	}
	result.BusinessName, result.EventName = snapshot.BusinessName, snapshot.EventName
	result.PeriodStart, result.PeriodEnd, result.Timezone = snapshot.PeriodStart, snapshot.PeriodEnd, snapshot.Timezone
	result.GeneratedAt, result.Source = snapshot.GeneratedAt, snapshot.Source
	result.VerificationStatus, result.VerificationLabel = snapshot.VerificationStatus, snapshot.VerificationLabel
	result.NetSales, result.TransactionCount, result.UnitsNet = snapshot.NetSales, snapshot.Transactions, snapshot.UnitsNet
	result.AverageTransaction, result.Daily = snapshot.AverageTransaction, snapshot.Daily
	result.CompletenessNote = snapshot.Completeness.Note
	return result, nil
}
