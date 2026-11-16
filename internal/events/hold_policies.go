package events

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"kitakarsa.com/tenantku/backend/internal/booking"
)

// StoredHoldPolicies resolves the platform-wide version active at the pricing timestamp.
type StoredHoldPolicies struct{}

func (StoredHoldPolicies) ForHold(ctx context.Context, tx pgx.Tx, _ string, _ int64, at time.Time) (HoldPolicy, error) {
	var policy HoldPolicy
	err := tx.QueryRow(ctx, `
		SELECT version, refund_policy, refund_terms, platform_fee, provider_fee
		FROM hold_policy_versions WHERE effective_at <= $1
		ORDER BY effective_at DESC LIMIT 1`, at).
		Scan(&policy.Version, &policy.Refund, &policy.RefundTerms, &policy.PlatformFee, &policy.ProviderFee)
	if errors.Is(err, pgx.ErrNoRows) {
		return HoldPolicy{}, booking.ErrHoldUnavailable
	}
	return policy, err
}
