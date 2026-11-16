package events

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"kitakarsa.com/tenantku/backend/internal/booking"
)

type HoldStand struct {
	ID                 string
	BlockID            *string
	AllowedCategoryIDs []string
}

type HoldInventory interface {
	// ActiveStands returns the active capacity for this event and slot, including
	// stands with holds or reservations. IDs are unique canonical UUIDs. An empty
	// category list allows all.
	ActiveStands(ctx context.Context, tx pgx.Tx, eventID, slotID string) ([]HoldStand, error)
}

type HoldPolicy struct {
	Version     string
	Refund      json.RawMessage
	PlatformFee int64
	ProviderFee int64
}

type HoldPolicies interface {
	// ForHold resolves the fees and effective refund policy at the same as-of time
	// used to compute the rent.
	ForHold(ctx context.Context, tx pgx.Tx, eventID string, rent int64, at time.Time) (HoldPolicy, error)
}

type HoldOfferSource struct {
	Inventory HoldInventory
	Policies  HoldPolicies
}

var _ booking.OfferSource = HoldOfferSource{}

func (source HoldOfferSource) ResolveOffer(ctx context.Context, tx pgx.Tx, request booking.OfferRequest) (booking.Offer, error) {
	if source.Inventory == nil || source.Policies == nil {
		return booking.Offer{}, booking.ErrHoldUnavailable
	}
	inventory, err := source.Inventory.ActiveStands(ctx, tx, request.EventID, request.SlotID)
	if err != nil {
		return booking.Offer{}, err
	}
	ids := make([]string, 0, len(inventory))
	var selected *HoldStand
	for i := range inventory {
		stand := &inventory[i]
		ids = append(ids, stand.ID)
		if stand.ID == request.StandID {
			selected = stand
		}
	}
	if selected == nil || (len(selected.AllowedCategoryIDs) > 0 && !slices.Contains(selected.AllowedCategoryIDs, request.CategoryID)) {
		return booking.Offer{}, booking.ErrNotBookable
	}

	active, err := loadPriceVersion(ctx, tx, activePricingQuery, request.EventID, request.Now)
	if err != nil {
		return booking.Offer{}, err
	}
	if active == nil {
		return booking.Offer{}, booking.ErrNotBookable
	}
	revision, err := strconv.Atoi(active.Revision)
	if err != nil {
		return booking.Offer{}, err
	}

	// Count only stands that are still part of this slot's active inventory.
	// A temporary hold or an allocation from any other slot cannot change its price.
	var confirmed int32
	err = tx.QueryRow(ctx, `SELECT count(*) FROM allocations a JOIN bookings b ON b.id = a.booking_id
		WHERE b.event_id = $1 AND a.event_slot_id = $2 AND a.stand_id = ANY($3::text[]::uuid[])
		AND a.status = 'reserved' AND b.status = 'confirmed'`, request.EventID, request.SlotID, ids).Scan(&confirmed)
	if err != nil {
		return booking.Offer{}, err
	}
	quote, err := active.Rules.quote(priceTarget{slotID: request.SlotID, stand: selected}, confirmed, int32(len(ids)))
	if err != nil {
		var fields fieldErrors
		if errors.As(err, &fields) && fields["event_slot_id"] != "" {
			return booking.Offer{}, booking.ErrNotBookable
		}
		return booking.Offer{}, err
	}
	policy, err := source.Policies.ForHold(ctx, tx, request.EventID, quote.Rent, request.Now)
	if err != nil {
		return booking.Offer{}, err
	}

	offer := booking.Offer{
		BlockID: selected.BlockID, PriceRevision: revision, Rent: quote.Rent,
		PlatformFee: policy.PlatformFee, ProviderFee: policy.ProviderFee,
		PolicyVersion: policy.Version, RefundPolicy: policy.Refund,
	}
	for _, quota := range active.Rules.CategoryQuotas {
		if quota.CategoryID != request.CategoryID {
			continue
		}
		cap := int(*quota.Cap)
		if quota.BlockID == "" {
			offer.EventQuota = &cap
		} else if selected.BlockID != nil && quota.BlockID == *selected.BlockID {
			offer.BlockQuota = &cap
		}
	}
	return offer, nil
}
