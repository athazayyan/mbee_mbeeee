package events

import (
	"strings"
	"testing"
)

const (
	blockA = "0b0c0000-0000-4000-8000-000000000001"
	standA = "57a0d000-0000-4000-8000-000000000001"
	standB = "57a0d000-0000-4000-8000-000000000002"
)

func validLayout() layoutInput {
	block := blockA
	return layoutInput{
		Width: 12, Height: 10, CellMeters: 1.5,
		Blocks: []layoutBlock{{ID: blockA, Label: "A · Kuliner", layoutRect: layoutRect{X: 0, Y: 0, Width: 6, Height: 4}}},
		Areas:  []layoutArea{{Kind: "walkway", layoutRect: layoutRect{X: 6, Y: 0, Width: 2, Height: 10}}},
		Stands: []layoutStand{
			{ID: standA, BlockID: &block, Code: "a-01", Active: true, layoutRect: layoutRect{X: 0, Y: 1, Width: 2, Height: 2}},
			{ID: standB, Code: "B-01", Active: true, layoutRect: layoutRect{X: 8, Y: 1, Width: 2, Height: 2}},
		},
	}
}

func TestLayoutValidateAcceptsValidLayout(t *testing.T) {
	input := validLayout()
	if fields := input.validate(); fields != nil {
		t.Fatalf("validate() = %v, want nil", fields)
	}
	if input.Stands[0].Code != "A-01" {
		t.Fatalf("code = %q, want normalized A-01", input.Stands[0].Code)
	}
	if input.Stands[1].CategoryIDs == nil || input.Stands[1].Facilities == nil {
		t.Fatal("nil slices must become empty arrays")
	}
}

func TestLayoutValidateRejectsInvalidLayouts(t *testing.T) {
	cases := map[string]struct {
		change func(*layoutInput)
		field  string
		text   string
	}{
		"grid too small":      {func(l *layoutInput) { l.Width = 3 }, "width", "petak"},
		"cell meters":         {func(l *layoutInput) { l.CellMeters = 0.2 }, "cell_meters", "meter"},
		"stand overlap":       {func(l *layoutInput) { l.Stands[1].X, l.Stands[1].Y = 1, 2 }, "stands", "tumpang tindih"},
		"stand over walkway":  {func(l *layoutInput) { l.Stands[1].X = 5 }, "stands", "menimpa lorong"},
		"stand out of grid":   {func(l *layoutInput) { l.Stands[1].X = 11 }, "stands", "keluar"},
		"rotated out of grid": {func(l *layoutInput) { l.Stands[1].X, l.Stands[1].Width, l.Stands[1].Rotation = 11, 1, 90 }, "stands", "keluar"},
		"duplicate code":      {func(l *layoutInput) { l.Stands[1].Code = "A-01" }, "stands", "dipakai lebih"},
		"duplicate id":        {func(l *layoutInput) { l.Stands[1].ID = standA }, "stands", "ID unik"},
		"bad rotation":        {func(l *layoutInput) { l.Stands[1].Rotation = 45 }, "stands", "Rotasi"},
		"unknown block":       {func(l *layoutInput) { other := standB; l.Stands[1].BlockID = &other }, "stands", "Blok untuk"},
		"blocks overlap": {func(l *layoutInput) {
			l.Blocks = append(l.Blocks, layoutBlock{ID: standB, Label: "B", layoutRect: layoutRect{X: 2, Y: 2, Width: 3, Height: 3}})
		}, "blocks", "bertumpuk"},
		"empty block label":     {func(l *layoutInput) { l.Blocks[0].Label = "  " }, "blocks", "nama blok"},
		"unknown area kind":     {func(l *layoutInput) { l.Areas[0].Kind = "parking" }, "areas", "jenis area"},
		"too many facilities":   {func(l *layoutInput) { l.Stands[0].Facilities = make([]string, 21) }, "stands", "maksimal 20"},
		"invalid category uuid": {func(l *layoutInput) { l.Stands[0].CategoryIDs = []string{"bukan-uuid"} }, "stands", "Kategori"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			input := validLayout()
			tc.change(&input)
			fields := input.validate()
			if fields == nil || !strings.Contains(fields[tc.field], tc.text) {
				t.Fatalf("validate()[%q] = %q, want message containing %q (all: %v)", tc.field, fields[tc.field], tc.text, fields)
			}
		})
	}
}

func TestStandSpanFollowsRotation(t *testing.T) {
	stand := layoutStand{Rotation: 90, layoutRect: layoutRect{X: 1, Y: 2, Width: 3, Height: 1}}
	if got := stand.span(); got != (layoutRect{X: 1, Y: 2, Width: 1, Height: 3}) {
		t.Fatalf("span() = %+v, want width and height swapped", got)
	}
}

func TestLayoutETag(t *testing.T) {
	if got := layoutETag(0); got != `"0"` {
		t.Fatalf("layoutETag(0) = %s", got)
	}
}
