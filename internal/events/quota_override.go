package events

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

var errQuotaOverrideForbidden = errors.New("quota override requires platform admin")

func verifyQuotaOverride(ctx context.Context, tx pgx.Tx, actor, eventID string, effective time.Time, input priceInput) error {
	var admin string
	err := tx.QueryRow(ctx, `SELECT user_id FROM platform_admins WHERE user_id = $1`, actor).Scan(&admin)
	if errors.Is(err, pgx.ErrNoRows) {
		return errQuotaOverrideForbidden
	}
	if err != nil {
		return err
	}
	if !httpapi.ValidSingleLine(strings.TrimSpace(*input.QuotaOverrideReason), 500) || input.CategoryQuotas == nil {
		return fieldErrors{"quota_override_reason": "Sertakan alasan 1–500 karakter dan aturan kuota."}
	}
	previous, err := loadPriceVersion(ctx, tx, activePricingQuery, eventID, effective)
	if err != nil {
		return err
	}
	if previous == nil || input.QuotaBaseVersion != previous.Revision {
		return fieldErrors{"category_quotas": "Versi kuota sudah berubah. Muat ulang atau pilih waktu berlaku yang sesuai."}
	}
	return nil
}

func writeQuotaOverrideAudit(ctx context.Context, tx pgx.Tx, actor, eventID, requestID string, effective time.Time, input priceInput, revision string) error {
	_, err := tx.Exec(ctx, `INSERT INTO admin_audit (actor_id, action, resource_type, resource_id, reason, before, after, request_id)
		SELECT $1, 'override_category_quotas', 'event', $2, btrim($3),
			jsonb_build_object('price_version', $4::text, 'category_quotas', COALESCE(p.rules->'category_quotas', '[]'::jsonb)),
			jsonb_build_object('price_version', $5::text, 'category_quotas', $6::jsonb, 'effective_at', $7::timestamptz), $8
		FROM event_pricing_versions p WHERE p.event_id = $2 AND p.revision::text = $4`,
		actor, eventID, *input.QuotaOverrideReason, input.QuotaBaseVersion, revision, input.CategoryQuotas, effective, requestID)
	return err
}
