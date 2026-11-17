package events

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"
	"time"
)

// SeedBlock is one zone of a demo event: Stands 2x2 stands, each with the zone's base price in rupiah.
type SeedBlock struct {
	Label  string
	Stands int
	Price  int64
}

// SeedEvent describes a demo event for SeedPublished.
type SeedEvent struct {
	Name, Slug, City, Address, Description, Instagram, Terms string
	Facilities                                               []string
	StartsAt, EndsAt                                         time.Time
	Blocks                                                   []SeedBlock
	Curated                                                  bool
}

// SeedStand is a stand that SeedPublished created, so seed data can refer to it.
type SeedStand struct{ ID, Code, Block string }

func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// seedStandTypes picks stand types for a demo zone from its label, cycled so a zone looks like a real bazar row:
// mostly the zone's main type with a few neighbours (a dessert stand between food stalls, a photobooth in a craft row).
func seedStandTypes(label string) []string {
	lower := strings.ToLower(label)
	switch {
	case strings.Contains(lower, "kopi") || strings.Contains(lower, "minuman"):
		return []string{"minuman", "minuman", "dessert"}
	case strings.Contains(lower, "jajanan") || strings.Contains(lower, "pendamping"):
		return []string{"dessert", "fresh_product", "dessert", "makanan_berat"}
	case strings.Contains(lower, "oleh"):
		return []string{"fresh_product", "aksesoris", "dessert"}
	case strings.Contains(lower, "kreatif"):
		return []string{"aksesoris", "photobooth", "beauty", "pakaian"}
	case strings.Contains(lower, "fesyen") || strings.Contains(lower, "kriya"):
		return []string{"pakaian", "aksesoris", "beauty", "pakaian"}
	default:
		return []string{"makanan_berat", "makanan_berat", "minuman", "makanan_berat", "dessert", "makanan_berat"}
	}
}

// SeedLayout builds a walkable demo layout: an entrance row, a walkway, then one block per zone with 2x2 stands
// in rows of three. It returns the layout and the stands in it.
func seedLayout(blocks []SeedBlock) (layoutInput, []SeedStand, []blockPrice, error) {
	const perRow, blockWidth, gap = 3, 6, 2
	var (
		layout  = layoutInput{CellMeters: 2}
		stands  []SeedStand
		prices  []blockPrice
		maxRows int
	)
	for i, spec := range blocks {
		rows := (spec.Stands + perRow - 1) / perRow
		maxRows = max(maxRows, rows)
		blockID, err := newUUID()
		if err != nil {
			return layout, nil, nil, err
		}
		price := spec.Price
		prices = append(prices, blockPrice{BlockID: blockID, BasePrice: &price})
		x := 1 + i*(blockWidth+gap)
		layout.Blocks = append(layout.Blocks, layoutBlock{ID: blockID, Label: spec.Label, layoutRect: layoutRect{X: x, Y: 3, Width: blockWidth, Height: 2 * rows}})
		letter := string(rune('A' + i))
		types := seedStandTypes(spec.Label)
		for n := 0; n < spec.Stands; n++ {
			standID, err := newUUID()
			if err != nil {
				return layout, nil, nil, err
			}
			code := fmt.Sprintf("%s%d", letter, n+1)
			block := blockID
			standType := types[n%len(types)]
			layout.Stands = append(layout.Stands, layoutStand{
				ID: standID, BlockID: &block, Code: code, Active: true, CategoryIDs: []string{}, Facilities: []string{"Listrik 450 watt"},
				Type:       &standType,
				layoutRect: layoutRect{X: x + 2*(n%perRow), Y: 3 + 2*(n/perRow), Width: 2, Height: 2},
			})
			stands = append(stands, SeedStand{ID: standID, Code: code, Block: spec.Label})
		}
	}
	layout.Width = 1 + len(blocks)*(blockWidth+gap)
	layout.Height = 3 + 2*maxRows + 3
	layout.Areas = []layoutArea{
		{Kind: "entrance", layoutRect: layoutRect{X: 0, Y: 0, Width: 4, Height: 1}},
		{Kind: "exit", layoutRect: layoutRect{X: layout.Width - 4, Y: 0, Width: 4, Height: 1}},
		{Kind: "walkway", layoutRect: layoutRect{X: 0, Y: 1, Width: layout.Width, Height: 2}},
		{Kind: "stage", layoutRect: layoutRect{X: 1, Y: layout.Height - 2, Width: min(10, layout.Width-2), Height: 2}},
	}
	return layout, stands, prices, nil
}

// SeedPublished creates a demo event the way an organizer would (draft, layout, price, publish), so every rule of
// the real flow applies. The organization must belong to actor, who must own it.
func (s Store) SeedPublished(ctx context.Context, actor, organization string, in SeedEvent) (string, []SeedStand, error) {
	description, terms, instagram := in.Description, in.Terms, in.Instagram
	created, err := s.create(ctx, actor, organization, eventInput{
		Name: &in.Name, Slug: &in.Slug, StartsAt: &in.StartsAt, EndsAt: &in.EndsAt, City: &in.City, Address: &in.Address,
		Timezone: ptr("Asia/Jakarta"), Description: &description, Terms: &terms, Instagram: &instagram, Facilities: &in.Facilities,
	})
	if err != nil {
		return "", nil, err
	}
	layout, stands, prices, err := seedLayout(in.Blocks)
	if err != nil {
		return "", nil, err
	}
	if _, err := s.saveLayout(ctx, actor, created.ID, "", layout); err != nil {
		return "", nil, fmt.Errorf("layout %s: %w", in.Name, err)
	}
	rules := priceRules{DefaultBasePrice: prices[0].BasePrice, BlockPrices: prices}
	if _, err := s.DB.Exec(ctx, `INSERT INTO event_pricing_versions (event_id, revision, created_by, rules, effective_at)
		VALUES ($1, 1, $2, $3, clock_timestamp() - interval '1 minute')`, created.ID, actor, rules); err != nil {
		return "", nil, err
	}
	if _, err := s.publishDraft(ctx, actor, created.ID, LayoutInventory{}); err != nil {
		return "", nil, fmt.Errorf("publish %s: %w", in.Name, err)
	}
	if in.Curated {
		if _, err := s.DB.Exec(ctx, `UPDATE events SET registration_mode = 'curated' WHERE id = $1`, created.ID); err != nil {
			return "", nil, err
		}
	}
	return created.ID, stands, nil
}

func ptr[T any](v T) *T { return &v }
