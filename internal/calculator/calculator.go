// Package calculator holds the public break-even calculator (E12-01/02). It is a pure formula: nothing is
// stored, and every figure is an estimate from the numbers the visitor typed in.
package calculator

// Bounds keep every product inside the range a JSON number carries exactly (2^53-1).
const (
	MaxFixedCost = 1_000_000_000 // Rp1 miliar
	MaxUnitPrice = 5_000_000
	MaxUnits     = 10_000_000
	MaxDays      = 3650
)

// Input is what the visitor knows about the stall for one event.
type Input struct {
	FixedCost           int64 `json:"fixed_cost"`             // rent, transport, staff for the whole event
	UnitPrice           int64 `json:"unit_price"`             // selling price of one unit
	VariableCostPerUnit int64 `json:"variable_cost_per_unit"` // ingredients and packaging per unit
	EstimatedUnits      int64 `json:"estimated_units"`        // units the visitor expects to sell
	SellingDays         int64 `json:"selling_days"`
}

// Result is the estimate. The break-even fields are nil when the margin is not positive, because no number of
// sales covers the fixed cost then.
type Result struct {
	Status             string `json:"status"` // reachable or unreachable
	ContributionMargin int64  `json:"contribution_margin"`
	EstimatedProfit    int64  `json:"estimated_profit"`
	UnitsAtBreakEven   *int64 `json:"units_at_break_even"`
	RevenueAtBreakEven *int64 `json:"revenue_at_break_even"`
	DailyUnitTarget    *int64 `json:"daily_unit_target"`
}

// Validate returns a message per invalid field, or nil.
func Validate(in Input) map[string]string {
	fields := make(map[string]string)
	if in.FixedCost < 0 || in.FixedCost > MaxFixedCost {
		fields["fixed_cost"] = "Isi biaya tetap Rp0 sampai Rp1.000.000.000."
	}
	if in.UnitPrice < 1 || in.UnitPrice > MaxUnitPrice {
		fields["unit_price"] = "Isi harga jual per unit Rp1 sampai Rp5.000.000."
	}
	if in.VariableCostPerUnit < 0 || in.VariableCostPerUnit > MaxUnitPrice {
		fields["variable_cost_per_unit"] = "Isi biaya per unit Rp0 sampai Rp5.000.000."
	}
	if in.EstimatedUnits < 0 || in.EstimatedUnits > MaxUnits {
		fields["estimated_units"] = "Isi perkiraan penjualan 0 sampai 10.000.000 unit."
	}
	if in.SellingDays < 1 || in.SellingDays > MaxDays {
		fields["selling_days"] = "Isi jumlah hari berjualan 1 sampai 3.650."
	}
	if len(fields) == 0 {
		return nil
	}
	return fields
}

// ceilDiv rounds up so the target never understates what is needed.
func ceilDiv(a, b int64) int64 { return (a + b - 1) / b }

// Calculate applies the agreed formula. Input must pass Validate.
//
//	margin = price - variable cost
//	break-even units = ceil(fixed cost / margin), revenue = units * price, daily target = ceil(units / days)
//	estimated profit = estimated units * margin - fixed cost (negative means a loss)
func Calculate(in Input) Result {
	margin := in.UnitPrice - in.VariableCostPerUnit
	result := Result{
		Status:             "unreachable",
		ContributionMargin: margin,
		EstimatedProfit:    in.EstimatedUnits*margin - in.FixedCost,
	}
	if margin <= 0 {
		return result
	}
	units := ceilDiv(in.FixedCost, margin)
	revenue := units * in.UnitPrice
	daily := ceilDiv(units, in.SellingDays)
	result.Status = "reachable"
	result.UnitsAtBreakEven, result.RevenueAtBreakEven, result.DailyUnitTarget = &units, &revenue, &daily
	return result
}
