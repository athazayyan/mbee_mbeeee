package funds

import (
	"context"

	"github.com/jackc/pgx/v5"
	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

// WithdrawApplications withdraws applications still waiting for a decision when an event is cancelled (E09-05).
// Approved applications follow their booking, which cancel_event_bookings closes and refunds. It runs in the
// caller's transaction and calling it again changes nothing.
func WithdrawApplications(ctx context.Context, tx pgx.Tx, eventID, actor string) error {
	if !httpapi.ValidID(eventID) {
		return ErrNotFound
	}
	if _, err := tx.Exec(ctx, `INSERT INTO application_status_history (application_id, actor_id, from_status, to_status, reason)
		SELECT id, $2, 'submitted', 'withdrawn', 'event_cancelled' FROM applications
		WHERE event_id = $1 AND status = 'submitted'`, eventID, actor); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE applications SET status = 'withdrawn', updated_at = clock_timestamp()
		WHERE event_id = $1 AND status = 'submitted'`, eventID)
	return err
}
