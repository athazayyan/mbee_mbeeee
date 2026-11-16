package events

import "testing"

func TestQuoteUsesSlotAndHighestEligibleTier(t *testing.T) {
	base, slotPriceAmount := int64(150001), int64(100001)
	earlier, later := int32(1000), int32(5000)
	rules := priceRules{
		DefaultBasePrice: &base,
		SlotPrices:       []slotPrice{{EventSlotID: "slot", BasePrice: &slotPriceAmount}},
		OccupancyTiers: []occupancyTier{
			{OccupancyBPS: &later, MultiplierBPS: 15000},
			{OccupancyBPS: &earlier, MultiplierBPS: 11000},
		},
	}
	quote, err := rules.quote(priceTarget{slotID: "slot"}, 3, 6)
	if err != nil {
		t.Fatal(err)
	}
	if quote.BasePrice != 100001 || quote.Rent != 150002 || quote.OccupancyBPS != 5000 || quote.MultiplierBPS != 15000 {
		t.Fatalf("unexpected quote: %+v", quote)
	}
}

func TestQuoteAppliesBoundsBeforeCheckingRupiahRange(t *testing.T) {
	maximum := int64(maxRupiah)
	zero := int32(0)
	floor, cap := int64(10000), int64(25000)
	rules := priceRules{
		DefaultBasePrice: &maximum,
		OccupancyTiers:   []occupancyTier{{OccupancyBPS: &zero, MultiplierBPS: 2147483647}},
		Floor:            &floor,
		Cap:              &cap,
	}
	quote, err := rules.quote(priceTarget{}, 0, 5)
	if err != nil || quote.Rent != cap {
		t.Fatalf("quote = %+v, error = %v; want cap %d", quote, err, cap)
	}
	rules.Cap = nil
	if _, err := rules.quote(priceTarget{}, 0, 5); err == nil {
		t.Fatal("expected an error for rent beyond the supported range")
	}
	zeroPrice := int64(0)
	rules.DefaultBasePrice = &zeroPrice
	quote, err = rules.quote(priceTarget{}, 0, 5)
	if err != nil || quote.Rent != floor {
		t.Fatalf("quote = %+v, error = %v; want floor %d", quote, err, floor)
	}
}

func TestQuoteUsesStandThenBlockThenSlotThenEvent(t *testing.T) {
	eventPrice, slotAmount, blockAmount, free := int64(100), int64(80), int64(50), int64(0)
	blockID := "block"
	rules := priceRules{
		DefaultBasePrice: &eventPrice,
		SlotPrices:       []slotPrice{{EventSlotID: "slot", BasePrice: &slotAmount}},
		BlockPrices:      []blockPrice{{BlockID: blockID, BasePrice: &blockAmount}},
		StandPrices:      []standPrice{{StandID: "free-stand", BasePrice: &free}},
	}
	for _, tc := range []struct {
		name   string
		target priceTarget
		want   int64
	}{
		{"free stand overrides block", priceTarget{slotID: "slot", stand: &HoldStand{ID: "free-stand", BlockID: &blockID}}, 0},
		{"block overrides slot", priceTarget{slotID: "slot", stand: &HoldStand{ID: "other-stand", BlockID: &blockID}}, 50},
		{"slot overrides event", priceTarget{slotID: "slot", stand: &HoldStand{ID: "stand-without-block"}}, 80},
		{"event fallback", priceTarget{slotID: "other-slot"}, 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			quote, err := rules.quote(tc.target, 0, 1)
			if err != nil || quote.Rent != tc.want {
				t.Fatalf("quote %+v, err %v; want %d", quote, err, tc.want)
			}
		})
	}
}
