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
	quote, err := rules.quote("slot", 3, 6)
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
	quote, err := rules.quote("", 0, 5)
	if err != nil || quote.Rent != cap {
		t.Fatalf("quote = %+v, error = %v; want cap %d", quote, err, cap)
	}
	rules.Cap = nil
	if _, err := rules.quote("", 0, 5); err == nil {
		t.Fatal("expected an error for rent beyond the supported range")
	}
	zeroPrice := int64(0)
	rules.DefaultBasePrice = &zeroPrice
	quote, err = rules.quote("", 0, 5)
	if err != nil || quote.Rent != floor {
		t.Fatalf("quote = %+v, error = %v; want floor %d", quote, err, floor)
	}
}
