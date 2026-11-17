package payment

import (
	"context"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"
)

var (
	ErrUnknownMethod = errors.New("payment method is not available")
	// ErrAmountTooLarge guards the fee arithmetic; real rents are far below it.
	ErrAmountTooLarge = errors.New("amount is too large to price")
)

// feeBearer is who pays the per-method admin fee. PROD-03 puts it on the tenant.
const feeBearer = "tenant"

// maxFeeBase keeps (base + flat) * 10000 inside int64.
const maxFeeBase = int64(1) << 40

// Method is a payment method released through Midtrans with its admin fee.
type Method struct {
	Code         string
	Label        string
	Description  string
	SnapChannels []string
	ConfirmTypes []string
	FeeFlat      int64
	FeeBPS       int
	FeeSource    string
	RefundMode   string
	SortOrder    int
}

const methodColumns = `code, label, description, snap_channels, confirm_types, fee_flat, fee_bps, fee_source, refund_mode, sort_order`

func scanMethod(row pgx.Row) (Method, error) {
	var m Method
	err := row.Scan(&m.Code, &m.Label, &m.Description, &m.SnapChannels, &m.ConfirmTypes, &m.FeeFlat, &m.FeeBPS,
		&m.FeeSource, &m.RefundMode, &m.SortOrder)
	return m, err
}

// Fee returns the admin fee added to base so that, after Midtrans takes its flat fee and percentage
// from the gross amount, the platform still receives base. The result is rounded up to whole rupiah.
func (m Method) Fee(base int64) (int64, error) {
	if base < 0 || base > maxFeeBase {
		return 0, ErrAmountTooLarge
	}
	if m.FeeFlat == 0 && m.FeeBPS == 0 {
		return 0, nil
	}
	denominator := int64(10000 - m.FeeBPS)
	needed := (base + m.FeeFlat) * 10000
	gross := (needed + denominator - 1) / denominator
	return gross - base, nil
}

// Confirms reports whether a Midtrans payment_type belongs to this method.
func (m Method) Confirms(paymentType string) bool {
	return slices.Contains(m.ConfirmTypes, paymentType)
}

// enabledMethods lists the methods open to tenants, in display order.
func enabledMethods(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}) ([]Method, error) {
	rows, err := q.Query(ctx, `SELECT `+methodColumns+` FROM payment_methods WHERE enabled ORDER BY sort_order, code`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Method, error) { return scanMethod(row) })
}

// enabledMethod loads one enabled method.
func enabledMethod(ctx context.Context, q pgx.Tx, code string) (Method, error) {
	m, err := scanMethod(q.QueryRow(ctx, `SELECT `+methodColumns+` FROM payment_methods WHERE code = $1 AND enabled`, code))
	if errors.Is(err, pgx.ErrNoRows) {
		return Method{}, ErrUnknownMethod
	}
	return m, err
}
