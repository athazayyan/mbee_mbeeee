package booking

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

type holdQuoteInput struct {
	BusinessID  string `json:"business_id"`
	EventID     string `json:"event_id"`
	StandID     string `json:"stand_id"`
	EventSlotID string `json:"event_slot_id"`
}

type holdQuote struct {
	holdQuoteInput
	EventName     string    `json:"event_name"`
	EventTerms    string    `json:"event_terms"`
	PolicyVersion string    `json:"refund_policy_version"`
	RefundTerms   string    `json:"refund_terms"`
	Price         Price     `json:"price"`
	ServerNow     time.Time `json:"-"`
}

func (s Store) quoteHold(ctx context.Context, actor string, input holdQuoteInput, source OfferSource) (holdQuote, error) {
	if !httpapi.ValidID(actor) || !normalizeHoldIDs(&input.BusinessID, &input.EventID, &input.StandID, &input.EventSlotID) {
		return holdQuote{}, ErrHoldInput
	}
	result := holdQuote{holdQuoteInput: input}
	err := pgx.BeginTxFunc(ctx, s.DB, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var category string
		err := tx.QueryRow(ctx, `SELECT b.category_id FROM businesses b JOIN memberships m ON m.business_id = b.id
			WHERE b.id = $1 AND m.user_id = $2 AND m.role = 'owner'`, input.BusinessID, actor).Scan(&category)
		if err != nil {
			return err
		}
		var terms *string
		var ends time.Time
		err = tx.QueryRow(ctx, `SELECT e.name, e.terms, slot.ends_at, transaction_timestamp()
			FROM events e JOIN event_slots slot ON slot.event_id = e.id
			WHERE e.id = $1 AND slot.id = $2 AND e.status IN ('published', 'live')`, input.EventID, input.EventSlotID).
			Scan(&result.EventName, &terms, &ends, &result.ServerNow)
		if err != nil {
			return err
		}
		if terms == nil || strings.TrimSpace(*terms) == "" || !ends.After(result.ServerNow) {
			return ErrNotBookable
		}
		offer, err := source.ResolveOffer(ctx, tx, OfferRequest{
			EventID: input.EventID, StandID: input.StandID, SlotID: input.EventSlotID, CategoryID: category, Now: result.ServerNow,
		})
		if err != nil {
			return err
		}
		var taken bool
		err = tx.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM allocations a JOIN bookings b ON b.id = a.booking_id
			WHERE a.stand_id = $1 AND a.event_slot_id = $2 AND a.status IN ('hold', 'offer', 'reserved')
			AND NOT (a.status IN ('hold', 'offer') AND b.status IN ('held', 'payment_pending') AND b.expires_at <= $3)
		)`, input.StandID, input.EventSlotID, result.ServerNow).Scan(&taken)
		if err != nil {
			return err
		}
		if taken {
			return ErrStandUnavailable
		}
		if err := checkQuota(ctx, tx, input.EventID, category, result.ServerNow, offer); err != nil {
			return err
		}
		if !httpapi.ValidSingleLine(offer.PolicyVersion, 80) || strings.TrimSpace(offer.RefundTerms) == "" {
			return ErrHoldUnavailable
		}
		result.EventTerms, result.PolicyVersion, result.RefundTerms = *terms, offer.PolicyVersion, offer.RefundTerms
		result.Price, err = priceFromOffer(offer)
		return err
	})
	return result, err
}

func priceFromOffer(offer Offer) (Price, error) {
	const maxAmount = 1<<53 - 1
	for _, amount := range []int64{offer.Rent, offer.PlatformFee, offer.ProviderFee} {
		if amount < 0 || amount > maxAmount {
			return Price{}, ErrHoldUnavailable
		}
	}
	total := offer.Rent + offer.PlatformFee + offer.ProviderFee
	if total > maxAmount || offer.PriceRevision < 1 {
		return Price{}, ErrHoldUnavailable
	}
	return Price{
		Currency: "IDR", Version: strconv.Itoa(offer.PriceRevision), Total: total,
		Rent: offer.Rent, PlatformFee: offer.PlatformFee, ProviderFee: offer.ProviderFee,
	}, nil
}
