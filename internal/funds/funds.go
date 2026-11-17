// Package funds handles what happens to money and guests after a booking is paid (E09): check-in at the
// gate, refunds when the organizer cancels, disputes, and payouts to the organizer.
package funds

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"kitakarsa.com/tenantku/backend/internal/payment"
)

const (
	// PayoutWorkingDays is how many working days after the event's last day a payout may be requested.
	PayoutWorkingDays = 4
	// CheckInLead is how long before the event starts that staff may check guests in.
	CheckInLead = 2 * time.Hour
)

var (
	ErrNotFound         = pgx.ErrNoRows
	ErrNotConfirmed     = errors.New("booking is not confirmed")
	ErrOutsideWindow    = errors.New("outside the check-in period")
	ErrInvalid          = errors.New("invalid request")
	ErrAdminRequired    = errors.New("platform admin required")
	ErrState            = errors.New("state does not allow this action")
	ErrPayoutBlocked    = errors.New("payout is not available")
	ErrPayoutExists     = errors.New("a payout already exists for this event")
	ErrDisputeClosed    = errors.New("disputes are closed for this event")
	ErrDisputeOpen      = errors.New("a dispute is already open for this booking")
	ErrAmountTooHigh    = errors.New("amount exceeds what can still be refunded")
	ErrUnavailable      = errors.New("funds are not available")
	ErrIdempotencyReuse = errors.New("idempotency key reused with a different request")
)

// Store keeps the E09 state. Provider is optional: without it refunds wait for an operator.
type Store struct {
	DB       *pgxpool.Pool
	Provider *payment.Midtrans
}

func (s Store) Ready(ctx context.Context) error {
	_, err := s.DB.Exec(ctx, `SELECT 1 FROM booking_codes, check_ins, refunds, organization_bank_accounts,
		payouts, disputes, non_working_days LIMIT 0`)
	return err
}

// isStaff reports whether the account belongs to the organization that owns the event, as owner or member.
func isStaff(ctx context.Context, tx pgx.Tx, actor, eventID string) (bool, error) {
	var staff bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM events e JOIN memberships m ON m.organization_id = e.organization_id
		WHERE e.id = $1 AND m.user_id = $2)`, eventID, actor).Scan(&staff)
	return staff, err
}

// isOwner reports whether the account owns the organization that owns the event.
func isOwner(ctx context.Context, tx pgx.Tx, actor, eventID string) (bool, error) {
	var owner bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM events e JOIN memberships m ON m.organization_id = e.organization_id
		WHERE e.id = $1 AND m.user_id = $2 AND m.role = 'owner')`, eventID, actor).Scan(&owner)
	return owner, err
}

// requireAdmin checks platform admin rights inside the transaction that performs the action.
func requireAdmin(ctx context.Context, tx pgx.Tx, actor string) error {
	var admin bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM platform_admins WHERE user_id = $1)`, actor).Scan(&admin); err != nil {
		return err
	}
	if !admin {
		return ErrAdminRequired
	}
	return nil
}

func audit(ctx context.Context, tx pgx.Tx, actor, action, resourceType, resourceID, reason string, before, after any) error {
	b, err := json.Marshal(before)
	if err != nil {
		return err
	}
	a, err := json.Marshal(after)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO admin_audit (actor_id, action, resource_type, resource_id, reason, before, after, request_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, '')`, actor, action, resourceType, resourceID, reason, b, a)
	return err
}
