package booking

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

var (
	ErrApplicationInput = errors.New("invalid application input")
	ErrApplicationState = errors.New("application is no longer awaiting a decision")
	ErrApplicationDup   = errors.New("business already has a live application for this event")
	ErrNotCurated       = errors.New("event does not use curated registration")
	ErrApplicantBusy    = errors.New("applicant already has an active booking for this event")
)

type Application struct {
	ID                string     `json:"id"`
	EventID           string     `json:"event_id"`
	BusinessID        string     `json:"business_id"`
	Status            string     `json:"status"`
	PreferredStandIDs []string   `json:"preferred_stand_ids"`
	Reason            *string    `json:"reason,omitempty"`
	BookingID         *string    `json:"booking_id,omitempty"`
	OfferExpiresAt    *time.Time `json:"offer_expires_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
}

type ApplicationInput struct {
	BusinessID        string   `json:"business_id"`
	PreferredStandIDs []string `json:"preferred_stand_ids"`
}

type ApproveInput struct {
	StandID     string `json:"stand_id"`
	EventSlotID string `json:"event_slot_id"`
}

type Offered struct {
	ApplicationID string    `json:"application_id"`
	BookingID     string    `json:"booking_id"`
	ExpiresAt     time.Time `json:"expires_at"`
}

// An approved application reads as expired or withdrawn once its offer booking lapses or is cancelled.
const applicationColumns = `a.id, a.event_id, a.business_id,
	CASE WHEN a.status = 'approved' AND (b.status = 'expired' OR
			(b.status IN ('held', 'payment_pending') AND b.expires_at <= statement_timestamp())) THEN 'expired'
		WHEN a.status = 'approved' AND b.status = 'cancelled' THEN 'withdrawn'
		ELSE a.status END,
	a.preferred_stand_ids::text[], a.reason, a.booking_id,
	CASE WHEN a.status = 'approved' THEN b.expires_at END, a.created_at`

const applicationFrom = ` FROM applications a LEFT JOIN bookings b ON b.id = a.booking_id `

func scanApplication(row pgx.Row) (Application, error) {
	var a Application
	err := row.Scan(&a.ID, &a.EventID, &a.BusinessID, &a.Status, &a.PreferredStandIDs, &a.Reason,
		&a.BookingID, &a.OfferExpiresAt, &a.CreatedAt)
	if a.PreferredStandIDs == nil {
		a.PreferredStandIDs = []string{}
	}
	return a, err
}

func normalizeApplication(input *ApplicationInput) error {
	input.BusinessID = strings.ToLower(input.BusinessID)
	if !httpapi.ValidID(input.BusinessID) || len(input.PreferredStandIDs) > 5 {
		return ErrApplicationInput
	}
	seen := map[string]bool{}
	for i, id := range input.PreferredStandIDs {
		id = strings.ToLower(id)
		if !httpapi.ValidID(id) || seen[id] {
			return ErrApplicationInput
		}
		seen[id] = true
		input.PreferredStandIDs[i] = id
	}
	if input.PreferredStandIDs == nil {
		input.PreferredStandIDs = []string{}
	}
	return nil
}

func addHistory(ctx context.Context, tx pgx.Tx, id string, actor *string, from *string, to, reason string) error {
	_, err := tx.Exec(ctx, `INSERT INTO application_status_history (application_id, actor_id, from_status, to_status, reason)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''))`, id, actor, from, to, reason)
	return err
}

// Submit records an application. It never claims a stand or quota.
func (s Store) Submit(ctx context.Context, actor, eventID string, input ApplicationInput) (Application, error) {
	eventID = strings.ToLower(eventID)
	if !httpapi.ValidID(actor) || !httpapi.ValidID(eventID) || normalizeApplication(&input) != nil {
		return Application{}, ErrApplicationInput
	}
	var result Application
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var business string
		if err := tx.QueryRow(ctx, `SELECT b.id FROM businesses b JOIN memberships m ON m.business_id = b.id
			WHERE b.id = $1 AND m.user_id = $2 AND m.role = 'owner' FOR SHARE OF b, m`,
			input.BusinessID, actor).Scan(&business); err != nil {
			return err
		}
		var mode string
		var open bool
		if err := tx.QueryRow(ctx, `SELECT registration_mode, ends_at > clock_timestamp() FROM events
			WHERE id = $1 AND status IN ('published', 'live') FOR SHARE`, eventID).Scan(&mode, &open); err != nil {
			return err
		}
		if mode != "curated" {
			return ErrNotCurated
		}
		if !open {
			return ErrNotBookable
		}
		// An approval whose offer lapsed or was cancelled no longer blocks a new application.
		lapsed, err := tx.Query(ctx, `UPDATE applications a SET status = 'expired', updated_at = clock_timestamp()
			FROM bookings b WHERE a.booking_id = b.id AND a.event_id = $1 AND a.business_id = $2 AND a.status = 'approved'
			AND (b.status IN ('expired', 'cancelled') OR (b.status IN ('held', 'payment_pending')
				AND b.expires_at <= statement_timestamp())) RETURNING a.id`, eventID, business)
		if err != nil {
			return err
		}
		lapsedIDs, err := pgx.CollectRows(lapsed, pgx.RowTo[string])
		if err != nil {
			return err
		}
		for _, lapsedID := range lapsedIDs {
			from := "approved"
			if err := addHistory(ctx, tx, lapsedID, nil, &from, "expired", "offer_lapsed"); err != nil {
				return err
			}
		}
		var id string
		err = tx.QueryRow(ctx, `INSERT INTO applications (event_id, business_id, submitted_by, preferred_stand_ids)
			VALUES ($1, $2, $3, $4::uuid[]) RETURNING id`, eventID, business, actor, input.PreferredStandIDs).Scan(&id)
		if err != nil {
			var dbError *pgconn.PgError
			if errors.As(err, &dbError) && dbError.ConstraintName == "applications_live_idx" {
				return ErrApplicationDup
			}
			return err
		}
		if err := addHistory(ctx, tx, id, &actor, nil, "submitted", ""); err != nil {
			return err
		}
		result, err = scanApplication(tx.QueryRow(ctx, `SELECT `+applicationColumns+applicationFrom+`WHERE a.id = $1`, id))
		return err
	})
	return result, err
}

// organizerGuard returns a SELECT fragment limiting events to those the actor owns as an organizer.
const organizerGuard = `EXISTS (SELECT 1 FROM memberships m WHERE m.user_id = $2 AND m.role = 'owner'
	AND m.organization_id = e.organization_id)`

// Approve allocates the stand as an offer and creates the tenant's booking in one transaction.
// The booking then follows the normal payment flow and expires through the same worker as holds.
func (s Store) Approve(ctx context.Context, actor, applicationID string, input ApproveInput, lifetime time.Duration, maxActive int, source OfferSource) (Offered, error) {
	applicationID, input.StandID, input.EventSlotID = strings.ToLower(applicationID), strings.ToLower(input.StandID), strings.ToLower(input.EventSlotID)
	if !httpapi.ValidID(actor) || !httpapi.ValidID(applicationID) || !httpapi.ValidID(input.StandID) ||
		!httpapi.ValidID(input.EventSlotID) {
		return Offered{}, ErrApplicationInput
	}
	if source == nil || lifetime < time.Second || maxActive < 1 {
		return Offered{}, ErrHoldUnavailable
	}
	var result Offered
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var eventID, business, applicant string
		if err := tx.QueryRow(ctx, `SELECT a.event_id, a.business_id, a.submitted_by FROM applications a
			JOIN events e ON e.id = a.event_id WHERE a.id = $1 AND `+organizerGuard, applicationID, actor).
			Scan(&eventID, &business, &applicant); err != nil {
			return err
		}
		// Same lock order as direct holds: event first, so approvals and holds serialize per event.
		var terms *string
		if err := tx.QueryRow(ctx, `SELECT terms FROM events WHERE id = $1 AND status IN ('published', 'live')
			FOR NO KEY UPDATE`, eventID).Scan(&terms); err != nil {
			return err
		}
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM applications WHERE id = $1 FOR UPDATE`, applicationID).Scan(&status); err != nil {
			return err
		}
		if status != "submitted" {
			return ErrApplicationState
		}
		if terms == nil || strings.TrimSpace(*terms) == "" {
			return ErrNotBookable
		}
		var category string
		if err := tx.QueryRow(ctx, `SELECT category_id FROM businesses WHERE id = $1`, business).Scan(&category); err != nil {
			return err
		}
		var now, ends time.Time
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp(), ends_at FROM event_slots
			WHERE id = $1 AND event_id = $2 FOR SHARE`, input.EventSlotID, eventID).Scan(&now, &ends); err != nil {
			return err
		}
		if !ends.After(now) {
			return ErrNotBookable
		}
		if _, err := expireHolds(ctx, tx, &eventID, nil); err != nil {
			return err
		}
		// An offer is a hold: the same one-active-hold-per-account-per-event rule applies, including pending payment.
		var active int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM bookings WHERE created_by = $1 AND event_id = $2
			AND status IN ('held', 'payment_pending') AND expires_at > $3`, applicant, eventID, now).Scan(&active); err != nil {
			return err
		}
		if active >= maxActive {
			return ErrApplicantBusy
		}
		offer, err := source.ResolveOffer(ctx, tx, OfferRequest{
			EventID: eventID, StandID: input.StandID, SlotID: input.EventSlotID, CategoryID: category, Now: now,
		})
		if err != nil {
			return err
		}
		if _, err := priceFromOffer(offer); err != nil {
			return err
		}
		if err := checkQuota(ctx, tx, eventID, category, now, offer); err != nil {
			return err
		}
		expires := now.Add(lifetime)
		if ends.Before(expires) {
			expires = ends
		}
		key := "application:" + applicationID
		digest := sha256.Sum256([]byte(key))
		booking, err := insertHold(ctx, tx, applicant, key, digest[:], HoldInput{
			BusinessID: business, EventID: eventID, StandID: input.StandID, EventSlotID: input.EventSlotID,
			AcceptedPolicyVersion: offer.PolicyVersion,
		}, category, *terms, expires, offer, "offer")
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE applications SET status = 'approved', booking_id = $2, decided_by = $3,
			decided_at = clock_timestamp(), updated_at = clock_timestamp() WHERE id = $1`,
			applicationID, booking.ID, actor); err != nil {
			return err
		}
		from := "submitted"
		if err := addHistory(ctx, tx, applicationID, &actor, &from, "approved", ""); err != nil {
			return err
		}
		result = Offered{ApplicationID: applicationID, BookingID: booking.ID, ExpiresAt: booking.ExpiresAt}
		return nil
	})
	return result, err
}

// Reject closes a submitted application with a reason shown to the applicant.
func (s Store) Reject(ctx context.Context, actor, applicationID, reason string) (Application, error) {
	applicationID, reason = strings.ToLower(applicationID), strings.TrimSpace(reason)
	if !httpapi.ValidID(actor) || !httpapi.ValidID(applicationID) || reason == "" || len([]rune(reason)) > 500 {
		return Application{}, ErrApplicationInput
	}
	var result Application
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var eventID string
		if err := tx.QueryRow(ctx, `SELECT a.event_id FROM applications a JOIN events e ON e.id = a.event_id
			WHERE a.id = $1 AND `+organizerGuard+` FOR UPDATE OF a`, applicationID, actor).Scan(&eventID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE applications SET status = 'rejected', reason = $2, decided_by = $3,
			decided_at = clock_timestamp(), updated_at = clock_timestamp()
			WHERE id = $1 AND status = 'submitted'`, applicationID, reason, actor)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrApplicationState
		}
		from := "submitted"
		if err := addHistory(ctx, tx, applicationID, &actor, &from, "rejected", reason); err != nil {
			return err
		}
		result, err = scanApplication(tx.QueryRow(ctx, `SELECT `+applicationColumns+applicationFrom+`WHERE a.id = $1`, applicationID))
		return err
	})
	return result, err
}

// ListForEvent shows an organizer every application for their event, newest first.
func (s Store) ListForEvent(ctx context.Context, actor, eventID string, limit int) ([]Application, error) {
	eventID = strings.ToLower(eventID)
	if !httpapi.ValidID(actor) || !httpapi.ValidID(eventID) {
		return nil, pgx.ErrNoRows
	}
	var owned bool
	if err := s.DB.QueryRow(ctx, `SELECT `+organizerGuard+` FROM events e WHERE e.id = $1`, eventID, actor).Scan(&owned); err != nil {
		return nil, err
	}
	if !owned {
		return nil, pgx.ErrNoRows
	}
	return s.queryApplications(ctx, `WHERE a.event_id = $1 ORDER BY a.created_at DESC, a.id DESC LIMIT $2`, eventID, limit)
}

// ListForBusiness shows a tenant the applications of a business they own.
func (s Store) ListForBusiness(ctx context.Context, actor, businessID string, limit int) ([]Application, error) {
	businessID = strings.ToLower(businessID)
	if !httpapi.ValidID(actor) || !httpapi.ValidID(businessID) {
		return nil, pgx.ErrNoRows
	}
	var owned string
	if err := s.DB.QueryRow(ctx, `SELECT business_id FROM memberships
		WHERE user_id = $1 AND business_id = $2 AND role = 'owner'`, actor, businessID).Scan(&owned); err != nil {
		return nil, err
	}
	return s.queryApplications(ctx, `WHERE a.business_id = $1 ORDER BY a.created_at DESC, a.id DESC LIMIT $2`, businessID, limit)
}

func (s Store) queryApplications(ctx context.Context, tail string, id string, limit int) ([]Application, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+applicationColumns+applicationFrom+tail, id, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Application, 0)
	for rows.Next() {
		a, err := scanApplication(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, a)
	}
	return result, rows.Err()
}

// SetRegistrationMode lets the organizer choose direct booking or curated applications.
func (s Store) SetRegistrationMode(ctx context.Context, actor, eventID, mode string) error {
	eventID = strings.ToLower(eventID)
	if !httpapi.ValidID(actor) || !httpapi.ValidID(eventID) || (mode != "direct" && mode != "curated") {
		return ErrApplicationInput
	}
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var current string
		if err := tx.QueryRow(ctx, `SELECT registration_mode FROM events e WHERE e.id = $1
			AND e.status IN ('draft', 'published', 'live') AND `+organizerGuard+` FOR NO KEY UPDATE OF e`,
			eventID, actor).Scan(&current); err != nil {
			return err
		}
		if current == mode {
			return nil
		}
		var live int
		if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM bookings WHERE event_id = $1
				AND status IN ('held', 'payment_pending', 'confirmed'))
			+ (SELECT count(*) FROM applications WHERE event_id = $1 AND status IN ('submitted', 'approved'))`,
			eventID).Scan(&live); err != nil {
			return err
		}
		if live > 0 {
			return ErrApplicationState // switching mid-sale would strand bookings or applications
		}
		_, err := tx.Exec(ctx, `UPDATE events SET registration_mode = $2 WHERE id = $1`, eventID, mode)
		return err
	})
}
