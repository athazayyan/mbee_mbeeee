package inventory

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"kitakarsa.com/tenantku/backend/internal/auth"
	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

var errDeviceRevoked = errors.New("device revoked")

type Device struct {
	ID           string     `json:"id"`
	BusinessID   string     `json:"business_id"`
	Label        *string    `json:"label,omitempty"`
	RegisteredAt time.Time  `json:"registered_at"`
	LastSeenAt   time.Time  `json:"last_seen_at"`
	LastSyncedAt *time.Time `json:"last_synced_at,omitempty"`
	RevokedAt    *time.Time `json:"revoked_at,omitempty"`
	RevokeReason *string    `json:"revoke_reason,omitempty"`
	OpenSession  bool       `json:"open_session"`
}

const deviceColumns = `d.id::text, d.business_id::text, d.label, d.registered_at, d.last_seen_at, d.last_synced_at,
	d.revoked_at, d.revoke_reason,
	EXISTS (SELECT 1 FROM pos_sessions p WHERE p.business_id = d.business_id AND p.device_id = d.id AND p.closed_at IS NULL)`

func scanDevice(row pgx.Row) (Device, error) {
	var d Device
	err := row.Scan(&d.ID, &d.BusinessID, &d.Label, &d.RegisteredAt, &d.LastSeenAt, &d.LastSyncedAt, &d.RevokedAt,
		&d.RevokeReason, &d.OpenSession)
	return d, err
}

func (s Store) devices(ctx context.Context, actor, business string) ([]Device, error) {
	var result []Device
	err := pgx.BeginTxFunc(ctx, s.DB, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if err := requireMember(ctx, tx, actor, business, false); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+deviceColumns+` FROM pos_devices d WHERE d.business_id = $1
			ORDER BY d.revoked_at IS NOT NULL, d.last_seen_at DESC LIMIT 200`, business)
		if err != nil {
			return err
		}
		result, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Device, error) { return scanDevice(row) })
		return err
	})
	return result, err
}

// setRevoked mencabut (dan menutup sesi terbuka) atau memulihkan perangkat. Hanya pemilik usaha.
func (s Store) setRevoked(ctx context.Context, actor, business, device string, revoke bool, reason *string) (Device, error) {
	var result Device
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if err := requireMember(ctx, tx, actor, business, true); err != nil {
			return err
		}
		var tag string
		var err error
		if revoke {
			err = tx.QueryRow(ctx, `UPDATE pos_devices SET revoked_at = COALESCE(revoked_at, clock_timestamp()),
				revoked_by = $3, revoke_reason = $4 WHERE business_id = $1 AND id = $2 RETURNING id::text`,
				business, device, actor, reason).Scan(&tag)
			if err == nil {
				_, err = tx.Exec(ctx, `UPDATE pos_sessions SET closed_at = clock_timestamp()
					WHERE business_id = $1 AND device_id = $2 AND closed_at IS NULL`, business, device)
			}
		} else {
			err = tx.QueryRow(ctx, `UPDATE pos_devices SET revoked_at = NULL, revoked_by = NULL, revoke_reason = NULL
				WHERE business_id = $1 AND id = $2 RETURNING id::text`, business, device).Scan(&tag)
		}
		if err != nil {
			return err
		}
		result, err = scanDevice(tx.QueryRow(ctx, `SELECT `+deviceColumns+` FROM pos_devices d
			WHERE d.business_id = $1 AND d.id = $2`, business, device))
		return err
	})
	return result, err
}

func (s Store) getDevices(c *gin.Context, account auth.Account) {
	business := c.Param("id")
	if !validID(c, business) {
		return
	}
	data, err := s.devices(c.Request.Context(), account.ID, strings.ToLower(business))
	if err != nil {
		writeError(c, err)
		return
	}
	if data == nil {
		data = []Device{}
	}
	httpapi.Success(c, http.StatusOK, data)
}

type revokeInput struct {
	Reason *string `json:"reason"`
}

func (s Store) revokeDevice(c *gin.Context, account auth.Account) {
	s.changeDevice(c, account, true)
}

func (s Store) restoreDevice(c *gin.Context, account auth.Account) {
	s.changeDevice(c, account, false)
}

func (s Store) changeDevice(c *gin.Context, account auth.Account, revoke bool) {
	business, device := c.Param("id"), c.Param("device")
	if !validID(c, business) || !validID(c, device) {
		return
	}
	var reason *string
	if revoke {
		input, ok := httpapi.ReadJSON[revokeInput](c, maxInput)
		if !ok {
			return
		}
		if input.Reason != nil {
			trimmed := strings.TrimSpace(*input.Reason)
			if trimmed != "" && !httpapi.ValidSingleLine(trimmed, 200) {
				writeError(c, fieldErrors{"reason": "Alasan satu baris hingga 200 karakter."})
				return
			}
			if trimmed != "" {
				reason = &trimmed
			}
		}
	}
	result, err := s.setRevoked(c.Request.Context(), account.ID, strings.ToLower(business), strings.ToLower(device), revoke, reason)
	if err != nil {
		writeError(c, err)
		return
	}
	httpapi.Success(c, http.StatusOK, result)
}
