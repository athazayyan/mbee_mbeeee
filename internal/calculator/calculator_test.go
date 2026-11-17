package calculator

import "testing"

func ptr(v int64) *int64 { return &v }

func TestCalculate(t *testing.T) {
	cases := []struct {
		name string
		in   Input
		want Result
	}{
		{"openapi example", Input{1_200_000, 20_000, 12_000, 150, 3},
			Result{"reachable", 8_000, 0, ptr(150), ptr(3_000_000), ptr(50)}},
		{"rounds break-even up", Input{1_000_000, 15_000, 5_000, 80, 4},
			Result{"reachable", 10_000, -200_000, ptr(100), ptr(1_500_000), ptr(25)}},
		{"rounds a non-whole result up", Input{90_000, 7_000, 3_000, 10, 3},
			Result{"reachable", 4_000, -50_000, ptr(23), ptr(161_000), ptr(8)}},
		{"no fixed cost breaks even at zero", Input{0, 10_000, 4_000, 5, 2},
			Result{"reachable", 6_000, 30_000, ptr(0), ptr(0), ptr(0)}},
		{"zero margin is unreachable", Input{500_000, 10_000, 10_000, 100, 2},
			Result{"unreachable", 0, -500_000, nil, nil, nil}},
		{"negative margin is unreachable", Input{500_000, 10_000, 12_000, 100, 2},
			Result{"unreachable", -2_000, -700_000, nil, nil, nil}},
	}
	for _, tc := range cases {
		got := Calculate(tc.in)
		if got.Status != tc.want.Status || got.ContributionMargin != tc.want.ContributionMargin || got.EstimatedProfit != tc.want.EstimatedProfit ||
			!same(got.UnitsAtBreakEven, tc.want.UnitsAtBreakEven) || !same(got.RevenueAtBreakEven, tc.want.RevenueAtBreakEven) ||
			!same(got.DailyUnitTarget, tc.want.DailyUnitTarget) {
			t.Errorf("%s: got %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func same(a, b *int64) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

func TestExtremesStayExact(t *testing.T) {
	// The largest allowed numbers still fit a JSON number exactly.
	got := Calculate(Input{MaxFixedCost, MaxUnitPrice, MaxUnitPrice - 1, MaxUnits, 1})
	if got.RevenueAtBreakEven == nil || *got.RevenueAtBreakEven != int64(MaxFixedCost)*MaxUnitPrice || *got.RevenueAtBreakEven > 1<<53-1 {
		t.Fatalf("revenue %+v", got)
	}
}

func TestValidate(t *testing.T) {
	if f := Validate(Input{1, 1, 0, 0, 1}); f != nil {
		t.Fatalf("minimal input rejected: %v", f)
	}
	f := Validate(Input{-1, 0, -1, -1, 0})
	for _, field := range []string{"fixed_cost", "unit_price", "variable_cost_per_unit", "estimated_units", "selling_days"} {
		if f[field] == "" {
			t.Errorf("%s accepted", field)
		}
	}
	if Validate(Input{MaxFixedCost + 1, 1, 0, 0, 1})["fixed_cost"] == "" || Validate(Input{0, MaxUnitPrice + 1, 0, 0, 1})["unit_price"] == "" {
		t.Error("upper bounds are not enforced")
	}
}
