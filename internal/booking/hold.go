package booking

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

var (
	ErrHoldInput           = errors.New("invalid hold input")
	ErrIdempotencyMismatch = errors.New("idempotency key already used with a different payload")
	ErrNotBookable         = errors.New("event or stand is not bookable")
	ErrHoldLimit           = errors.New("too many active holds")
	ErrStandUnavailable    = errors.New("stand is already allocated")
	ErrQuotaFull           = errors.New("category quota reached")
	ErrPolicyChanged       = errors.New("accepted refund policy is no longer active")
	ErrHoldUnavailable     = errors.New("hold configuration is unavailable")
)

type HoldInput struct {
	BusinessID            string `json:"business_id"`
	EventID               string `json:"event_id"`
	StandID               string `json:"stand_id"`
	EventSlotID           string `json:"event_slot_id"`
	AcceptedPolicyVersion string `json:"accepted_policy_version"`
}

type HoldSettings struct {
	Lifetime  time.Duration
	MaxActive int
}

type OfferRequest struct {
	EventID    string
	StandID    string
	SlotID     string
	CategoryID string
	Now        time.Time
}

type Offer struct {
	BlockID       *string
	EventQuota    *int
	BlockQuota    *int
	PriceRevision int
	Rent          int64
	PlatformFee   int64
	ProviderFee   int64
	PolicyVersion string
	RefundPolicy  json.RawMessage
}

// OfferSource resolves a stand, price, category caps, and refund policy from
// server data in the supplied transaction. The event lock is already held.
type OfferSource interface {
	ResolveOffer(context.Context, pgx.Tx, OfferRequest) (Offer, error)
}

func (s Store) CreateHold(ctx context.Context, actor, key string, input HoldInput, settings HoldSettings, source OfferSource) (Booking, error) {
	result, _, err := s.createHold(ctx, actor, key, input, settings, source)
	return result, err
}

func (settings HoldSettings) valid(source OfferSource) bool {
	return source != nil && settings.Lifetime >= time.Second && settings.MaxActive > 0
}

func validHoldKey(key string) bool {
	return len(key) <= 128 && strings.TrimSpace(key) == key && httpapi.ValidSingleLine(key, 128)
}

func (s Store) createHold(ctx context.Context, actor, key string, input HoldInput, settings HoldSettings, source OfferSource) (Booking, bool, error) {
	if !settings.valid(source) {
		return Booking{}, false, ErrHoldUnavailable
	}
	for _, id := range []*string{&input.BusinessID, &input.EventID, &input.StandID, &input.EventSlotID} {
		*id = strings.ToLower(*id)
		if !httpapi.ValidID(*id) {
			return Booking{}, false, ErrHoldInput
		}
	}
	if !httpapi.ValidID(actor) || !validHoldKey(key) || !httpapi.ValidSingleLine(input.AcceptedPolicyVersion, 80) {
		return Booking{}, false, ErrHoldInput
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return Booking{}, false, err
	}
	digest := sha256.Sum256(payload)

	var result Booking
	var created bool
	err = pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		// Serialize hold limits and idempotency by actor, followed by all changes
		// to allocations and category quotas for this event.
		// Use a transaction lock so the booking role needs no UPDATE grant on users.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(
			hashtextextended('tenantku.booking.hold:' || $1::uuid::text, 0))`, actor); err != nil {
			return err
		}
		var category string
		if err := tx.QueryRow(ctx, `SELECT b.category_id FROM businesses b JOIN memberships m ON m.business_id = b.id
			WHERE b.id = $1 AND m.user_id = $2 AND m.role = 'owner' FOR SHARE OF b, m`,
			input.BusinessID, actor).Scan(&category); err != nil {
			return err
		}

		var priorID string
		var priorDigest []byte
		err := tx.QueryRow(ctx, `SELECT id, request_digest FROM bookings WHERE created_by = $1 AND idempotency_key = $2`, actor, key).
			Scan(&priorID, &priorDigest)
		if err == nil {
			if !bytes.Equal(priorDigest, digest[:]) {
				return ErrIdempotencyMismatch
			}
			result, err = scanBooking(tx.QueryRow(ctx, `SELECT `+bookingColumns+` FROM bookings b
				JOIN allocations a ON a.booking_id = b.id JOIN events e ON e.id = b.event_id WHERE b.id = $1`, priorID))
			return err
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		var terms *string
		if err := tx.QueryRow(ctx, `SELECT terms FROM events WHERE id = $1
			AND status IN ('published', 'live') FOR NO KEY UPDATE`, input.EventID).Scan(&terms); err != nil {
			return err
		}
		if terms == nil || strings.TrimSpace(*terms) == "" {
			return ErrNotBookable
		}
		var now, ends time.Time
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp(), ends_at FROM event_slots
			WHERE id = $1 AND event_id = $2 FOR SHARE`, input.EventSlotID, input.EventID).Scan(&now, &ends); err != nil {
			return err
		}
		if !ends.After(now) {
			return ErrNotBookable
		}
		if _, err := expireHolds(ctx, tx, &input.EventID, nil); err != nil {
			return err
		}
		var active int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM bookings
			WHERE created_by = $1 AND status IN ('held', 'payment_pending') AND expires_at > $2`,
			actor, now).Scan(&active); err != nil {
			return err
		}
		if active >= settings.MaxActive {
			return ErrHoldLimit
		}

		offer, err := source.ResolveOffer(ctx, tx, OfferRequest{
			EventID: input.EventID, StandID: input.StandID, SlotID: input.EventSlotID, CategoryID: category, Now: now,
		})
		if err != nil {
			return err
		}
		if offer.PolicyVersion != input.AcceptedPolicyVersion {
			return ErrPolicyChanged
		}
		if err := checkQuota(ctx, tx, input, category, now, offer); err != nil {
			return err
		}
		expires := now.Add(settings.Lifetime)
		if ends.Before(expires) {
			expires = ends
		}
		result, err = insertHold(ctx, tx, actor, key, digest[:], input, category, *terms, expires, offer)
		created = err == nil
		return err
	})
	return result, created && err == nil, err
}

func checkQuota(ctx context.Context, tx pgx.Tx, input HoldInput, category string, now time.Time, offer Offer) error {
	if offer.EventQuota == nil && offer.BlockQuota == nil {
		return nil
	}
	if offer.BlockQuota != nil && (offer.BlockID == nil || !httpapi.ValidID(*offer.BlockID)) {
		return ErrHoldUnavailable
	}
	var eventCount, blockCount int
	err := tx.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE a.block_id = $4::uuid)
		FROM bookings b JOIN allocations a ON a.booking_id = b.id
		WHERE b.event_id = $1 AND b.category_id = $2 AND a.status IN ('hold', 'offer', 'reserved')
		AND (b.status = 'confirmed' OR (b.status IN ('held', 'payment_pending') AND b.expires_at > $3))`,
		input.EventID, category, now, offer.BlockID).Scan(&eventCount, &blockCount)
	if err != nil {
		return err
	}
	if (offer.EventQuota != nil && eventCount >= *offer.EventQuota) ||
		(offer.BlockQuota != nil && blockCount >= *offer.BlockQuota) {
		return ErrQuotaFull
	}
	return nil
}

func insertHold(ctx context.Context, tx pgx.Tx, actor, key string, digest []byte, input HoldInput,
	category, terms string, expires time.Time, offer Offer) (Booking, error) {
	var id string
	err := tx.QueryRow(ctx, `INSERT INTO bookings (business_id, event_id, category_id, created_by, status, expires_at,
		price_revision, rent, platform_fee, provider_fee, terms_snapshot, refund_policy_version,
		refund_policy_snapshot, idempotency_key, request_digest)
		VALUES ($1, $2, $3, $4, 'held', $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		RETURNING id`, input.BusinessID, input.EventID, category, actor, expires, offer.PriceRevision,
		offer.Rent, offer.PlatformFee, offer.ProviderFee, terms, offer.PolicyVersion, offer.RefundPolicy, key, digest).Scan(&id)
	if err != nil {
		return Booking{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO allocations (booking_id, event_id, event_slot_id, stand_id, block_id, status)
		VALUES ($1, $2, $3, $4, $5, 'hold')`, id, input.EventID, input.EventSlotID, input.StandID, offer.BlockID)
	if err != nil {
		var dbError *pgconn.PgError
		if errors.As(err, &dbError) && dbError.ConstraintName == "allocations_active_stand_slot_idx" {
			return Booking{}, ErrStandUnavailable
		}
		return Booking{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO booking_status_history (booking_id, actor_id, to_status) VALUES ($1, $2, 'held')`,
		id, actor); err != nil {
		return Booking{}, err
	}
	return scanBooking(tx.QueryRow(ctx, `SELECT `+bookingColumns+` FROM bookings b
		JOIN allocations a ON a.booking_id = b.id JOIN events e ON e.id = b.event_id WHERE b.id = $1`, id))
}
