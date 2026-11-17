package payment

import "testing"

func TestMethodFeeKeepsBase(t *testing.T) {
	for _, m := range []Method{
		{Code: "va", FeeFlat: 4000},
		{Code: "qris", FeeBPS: 70},
		{Code: "gopay", FeeBPS: 200},
		{Code: "card", FeeFlat: 2000, FeeBPS: 290},
		{Code: "free"},
	} {
		for _, base := range []int64{1, 999, 150000, 1234567, 99999999} {
			fee, err := m.Fee(base)
			if err != nil {
				t.Fatal(err)
			}
			gross := base + fee
			// Midtrans takes the flat fee plus the percentage of the gross amount.
			kept := gross - m.FeeFlat - gross*int64(m.FeeBPS)/10000
			if fee < 0 || (m.FeeFlat+int64(m.FeeBPS) > 0 && fee == 0) {
				t.Errorf("%s base %d: fee %d", m.Code, base, fee)
			}
			// Rounding the percentage down on the provider side may leave at most one rupiah either way.
			if kept < base-1 || (fee > 0 && kept > base+int64(m.FeeBPS)/100+2) {
				t.Errorf("%s base %d gross %d keeps %d", m.Code, base, gross, kept)
			}
		}
	}
	if _, err := (Method{FeeFlat: 1}).Fee(maxFeeBase + 1); err != ErrAmountTooLarge {
		t.Fatalf("oversized base: %v", err)
	}
}
