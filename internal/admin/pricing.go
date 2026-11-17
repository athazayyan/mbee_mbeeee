package admin

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"kitakarsa.com/tenantku/backend/internal/platform/adminperm"
	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

var errConflict = errors.New("conflicting change")

// MethodFee is a payment method with the fee a tenant pays on top of the rent (E16-03).
// A change only affects attempts created after it: every attempt keeps the fee and fee_version it was created with.
type MethodFee struct {
	Code       string    `json:"code"`
	Label      string    `json:"label"`
	Enabled    bool      `json:"enabled"`
	FeeFlat    int64     `json:"fee_flat"`
	FeeBPS     int       `json:"fee_bps"`
	FeeSource  string    `json:"fee_source"`
	FeeVersion string    `json:"fee_version"`
	RefundMode string    `json:"refund_mode"`
	UpdatedAt  time.Time `json:"updated_at"`
}

const methodFeeColumns = `code, label, enabled, fee_flat, fee_bps, fee_source, fee_version, refund_mode, updated_at`

func scanMethodFee(row pgx.Row) (MethodFee, error) {
	var m MethodFee
	err := row.Scan(&m.Code, &m.Label, &m.Enabled, &m.FeeFlat, &m.FeeBPS, &m.FeeSource, &m.FeeVersion, &m.RefundMode, &m.UpdatedAt)
	return m, err
}

func (s Store) methodFees(ctx context.Context) ([]MethodFee, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+methodFeeColumns+` FROM payment_methods ORDER BY sort_order, code`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (MethodFee, error) { return scanMethodFee(row) })
}

type methodInput struct {
	Enabled    *bool   `json:"enabled"`
	FeeFlat    *int64  `json:"fee_flat"`
	FeeBPS     *int    `json:"fee_bps"`
	FeeSource  *string `json:"fee_source"`
	FeeVersion *string `json:"fee_version"`
	RefundMode *string `json:"refund_mode"`
	Reason     string  `json:"reason"`
}

var feeVersionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,39}$`)

func validReason(reason string) bool {
	return httpapi.ValidSingleLine(reason, 500)
}

func validateMethod(input *methodInput) map[string]string {
	fields := make(map[string]string)
	input.Reason = strings.TrimSpace(input.Reason)
	if !validReason(input.Reason) {
		fields["reason"] = "Tulis alasan 1 sampai 500 karakter untuk jejak audit."
	}
	if input.FeeFlat != nil && (*input.FeeFlat < 0 || *input.FeeFlat > 1_000_000) {
		fields["fee_flat"] = "Biaya tetap antara Rp0 dan Rp1.000.000."
	}
	if input.FeeBPS != nil && (*input.FeeBPS < 0 || *input.FeeBPS > 2000) {
		fields["fee_bps"] = "Persentase biaya antara 0 dan 2000 basis poin (20%)."
	}
	if input.FeeSource != nil && *input.FeeSource != "estimate" && *input.FeeSource != "contract" {
		fields["fee_source"] = "Pilih estimate atau contract."
	}
	if input.RefundMode != nil && *input.RefundMode != "api" && *input.RefundMode != "manual" {
		fields["refund_mode"] = "Pilih api atau manual."
	}
	if input.FeeVersion != nil && !feeVersionPattern.MatchString(*input.FeeVersion) {
		fields["fee_version"] = "Gunakan 1 sampai 40 karakter: huruf, angka, titik, garis bawah, atau tanda hubung."
	}
	if input.Enabled == nil && input.FeeFlat == nil && input.FeeBPS == nil && input.FeeSource == nil &&
		input.RefundMode == nil && input.FeeVersion == nil {
		fields["fee_flat"] = "Isi setidaknya satu perubahan."
	}
	if len(fields) == 0 {
		return nil
	}
	return fields
}

// fieldError reports invalid input back to the handler as a 422.
type fieldError map[string]string

func (e fieldError) Error() string { return "invalid input" }

// updateMethod changes a payment method. A new fee amount needs a new fee_version, so every attempt and ledger entry
// stays attributable to the fee it was charged under. Sending the same values again changes and audits nothing.
func (s Store) updateMethod(ctx context.Context, actor, code string, input methodInput, requestID string) (MethodFee, error) {
	var result MethodFee
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if err := requireAdmin(ctx, tx, actor, adminperm.PricingManage); err != nil {
			return err
		}
		before, err := scanMethodFee(tx.QueryRow(ctx, `SELECT `+methodFeeColumns+` FROM payment_methods WHERE code = $1 FOR UPDATE`, code))
		if err != nil {
			return err
		}
		next := before
		if input.Enabled != nil {
			next.Enabled = *input.Enabled
		}
		if input.FeeFlat != nil {
			next.FeeFlat = *input.FeeFlat
		}
		if input.FeeBPS != nil {
			next.FeeBPS = *input.FeeBPS
		}
		if input.FeeSource != nil {
			next.FeeSource = *input.FeeSource
		}
		if input.RefundMode != nil {
			next.RefundMode = *input.RefundMode
		}
		if input.FeeVersion != nil {
			next.FeeVersion = *input.FeeVersion
		}
		feeChanged := next.FeeFlat != before.FeeFlat || next.FeeBPS != before.FeeBPS
		if feeChanged && next.FeeVersion == before.FeeVersion {
			return fieldError{"fee_version": "Biaya berubah, jadi beri nama versi biaya yang baru."}
		}
		next.UpdatedAt = before.UpdatedAt
		if next == before {
			result = before
			return nil
		}
		result, err = scanMethodFee(tx.QueryRow(ctx, `UPDATE payment_methods SET enabled = $2, fee_flat = $3, fee_bps = $4,
			fee_source = $5, fee_version = $6, refund_mode = $7, updated_at = clock_timestamp() WHERE code = $1
			RETURNING `+methodFeeColumns, code, next.Enabled, next.FeeFlat, next.FeeBPS, next.FeeSource, next.FeeVersion, next.RefundMode))
		if err != nil {
			return err
		}
		return writeAudit(ctx, tx, auditRecord{actor: actor, action: "payment_method.update", resourceType: "payment_method",
			reason: input.Reason, requestID: requestID, before: auditMethod(before), after: auditMethod(result),
			resourceID: ""})
	})
	return result, err
}

func auditMethod(m MethodFee) map[string]any {
	return map[string]any{"code": m.Code, "enabled": m.Enabled, "fee_flat": m.FeeFlat, "fee_bps": m.FeeBPS,
		"fee_source": m.FeeSource, "fee_version": m.FeeVersion, "refund_mode": m.RefundMode}
}

// PlanVersion is one dated price list for event plans.
type PlanVersion struct {
	ID                   int       `json:"id"`
	EffectiveAt          time.Time `json:"effective_at"`
	CustomPricePerTenant int64     `json:"custom_price_per_tenant"`
	CreatedAt            time.Time `json:"created_at"`
	Current              bool      `json:"current"`
	Plans                []Plan    `json:"plans"`
}

func (s Store) planVersions(ctx context.Context) ([]PlanVersion, error) {
	rows, err := s.DB.Query(ctx, `SELECT v.id, v.effective_at, v.custom_price_per_tenant, v.created_at,
			v.id = (SELECT id FROM plan_price_versions WHERE effective_at <= now() ORDER BY effective_at DESC LIMIT 1)
		FROM plan_price_versions v ORDER BY v.effective_at DESC LIMIT 50`)
	if err != nil {
		return nil, err
	}
	versions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (PlanVersion, error) {
		var v PlanVersion
		err := row.Scan(&v.ID, &v.EffectiveAt, &v.CustomPricePerTenant, &v.CreatedAt, &v.Current)
		return v, err
	})
	if err != nil {
		return nil, err
	}
	for i := range versions {
		planRows, err := s.DB.Query(ctx, `SELECT code, name, capacity, price FROM plan_prices WHERE version_id = $1 ORDER BY capacity`, versions[i].ID)
		if err != nil {
			return nil, err
		}
		versions[i].Plans, err = pgx.CollectRows(planRows, func(row pgx.CollectableRow) (Plan, error) {
			var p Plan
			err := row.Scan(&p.Code, &p.Name, &p.Capacity, &p.Price)
			p.PerTenant = perTenant(p.Price, p.Capacity)
			return p, err
		})
		if err != nil {
			return nil, err
		}
	}
	return versions, nil
}

type planInput struct {
	Code     string `json:"code"`
	Name     string `json:"name"`
	Capacity int64  `json:"capacity"`
	Price    int64  `json:"price"`
}

type versionInput struct {
	EffectiveAt          time.Time   `json:"effective_at"`
	CustomPricePerTenant int64       `json:"custom_price_per_tenant"`
	Plans                []planInput `json:"plans"`
	Reason               string      `json:"reason"`
}

var planCodePattern = regexp.MustCompile(`^[a-z]{2,20}$`)

const maxMoney = int64(9007199254740991)

func validateVersion(input *versionInput, now time.Time) map[string]string {
	fields := make(map[string]string)
	input.Reason = strings.TrimSpace(input.Reason)
	if !validReason(input.Reason) {
		fields["reason"] = "Tulis alasan 1 sampai 500 karakter untuk jejak audit."
	}
	if input.EffectiveAt.IsZero() || !input.EffectiveAt.After(now) {
		fields["effective_at"] = "Waktu berlaku harus di masa depan agar harga lama tidak berubah."
	}
	if input.CustomPricePerTenant < 0 || input.CustomPricePerTenant > maxMoney {
		fields["custom_price_per_tenant"] = "Isi tarif per tenant tambahan dengan rupiah utuh."
	}
	if len(input.Plans) < 1 || len(input.Plans) > 12 {
		fields["plans"] = "Isi 1 sampai 12 paket."
		return fields
	}
	codes, capacities := map[string]bool{}, map[int64]bool{}
	for i, plan := range input.Plans {
		key := fmt.Sprintf("plans[%d]", i)
		switch {
		case !planCodePattern.MatchString(plan.Code):
			fields[key] = "Kode paket 2 sampai 20 huruf kecil."
		case !httpapi.ValidSingleLine(plan.Name, 40):
			fields[key] = "Nama paket 1 sampai 40 karakter."
		case plan.Capacity < 1 || plan.Capacity > 100000:
			fields[key] = "Kapasitas 1 sampai 100.000 tenant."
		case plan.Price < 1 || plan.Price > maxMoney:
			fields[key] = "Harga paket harus lebih dari Rp0."
		case codes[plan.Code] || capacities[plan.Capacity]:
			fields[key] = "Kode dan kapasitas paket tidak boleh kembar."
		}
		codes[plan.Code], capacities[plan.Capacity] = true, true
	}
	if len(fields) == 0 {
		return nil
	}
	return fields
}

// publishVersion adds a price list that takes effect at a future time. Existing versions are never edited, so purchases
// already made keep their price. Sending the same list for the same time again returns it; a different list is a conflict.
func (s Store) publishVersion(ctx context.Context, actor string, input versionInput, requestID string) (PlanVersion, bool, error) {
	var result PlanVersion
	created := false
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if err := requireAdmin(ctx, tx, actor, adminperm.PricingManage); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('tenantku.admin.plan_versions', 0))`); err != nil {
			return err
		}
		var existing int
		err := tx.QueryRow(ctx, `SELECT id FROM plan_price_versions WHERE effective_at = $1`, input.EffectiveAt).Scan(&existing)
		if err == nil {
			versions, err := s.planVersionByID(ctx, tx, existing)
			if err != nil {
				return err
			}
			if !sameVersion(versions, input) {
				return errConflict
			}
			result = versions
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var id int
		if err := tx.QueryRow(ctx, `INSERT INTO plan_price_versions (effective_at, custom_price_per_tenant, created_by)
			VALUES ($1, $2, $3) RETURNING id`, input.EffectiveAt, input.CustomPricePerTenant, actor).Scan(&id); err != nil {
			return err
		}
		for _, plan := range input.Plans {
			if _, err := tx.Exec(ctx, `INSERT INTO plan_prices (version_id, code, name, capacity, price) VALUES ($1, $2, $3, $4, $5)`,
				id, plan.Code, plan.Name, plan.Capacity, plan.Price); err != nil {
				return err
			}
		}
		result, err = s.planVersionByID(ctx, tx, id)
		if err != nil {
			return err
		}
		created = true
		var previous *PlanVersion
		if current, err := s.planVersionByID(ctx, tx, 0); err == nil {
			previous = &current
		}
		return writeAudit(ctx, tx, auditRecord{actor: actor, action: "plan_prices.publish", resourceType: "plan_price_version",
			reason: input.Reason, requestID: requestID, before: previous, after: result})
	})
	return result, created, err
}

// planVersionByID loads a version, or the one in force now when id is 0.
func (s Store) planVersionByID(ctx context.Context, tx pgx.Tx, id int) (PlanVersion, error) {
	var v PlanVersion
	err := tx.QueryRow(ctx, `SELECT id, effective_at, custom_price_per_tenant, created_at FROM plan_price_versions
		WHERE ($1 = 0 AND effective_at <= now()) OR id = $1 ORDER BY effective_at DESC LIMIT 1`, id).
		Scan(&v.ID, &v.EffectiveAt, &v.CustomPricePerTenant, &v.CreatedAt)
	if err != nil {
		return v, err
	}
	rows, err := tx.Query(ctx, `SELECT code, name, capacity, price FROM plan_prices WHERE version_id = $1 ORDER BY capacity`, v.ID)
	if err != nil {
		return v, err
	}
	v.Plans, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Plan, error) {
		var p Plan
		err := row.Scan(&p.Code, &p.Name, &p.Capacity, &p.Price)
		p.PerTenant = perTenant(p.Price, p.Capacity)
		return p, err
	})
	return v, err
}

func sameVersion(v PlanVersion, input versionInput) bool {
	if v.CustomPricePerTenant != input.CustomPricePerTenant || len(v.Plans) != len(input.Plans) {
		return false
	}
	want := slices.Clone(input.Plans)
	slices.SortFunc(want, func(a, b planInput) int { return int(a.Capacity - b.Capacity) })
	for i, plan := range v.Plans {
		if plan.Code != want[i].Code || plan.Name != want[i].Name || plan.Capacity != want[i].Capacity || plan.Price != want[i].Price {
			return false
		}
	}
	return true
}
