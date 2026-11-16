package events

import (
	"cmp"
	"context"
	"slices"

	"github.com/jackc/pgx/v5"
)

type pricingInventory struct {
	Stands []pricingStand `json:"stands"`
	Blocks []pricingBlock `json:"blocks"`
}

type pricingStand struct {
	ID      string   `json:"id"`
	Code    string   `json:"code"`
	BlockID *string  `json:"block_id,omitempty"`
	SlotIDs []string `json:"slot_ids"`
}

type pricingBlock struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

func readPricingInventory(ctx context.Context, tx pgx.Tx, source HoldInventory, eventID string, slots []pricedSlot) (*pricingInventory, error) {
	stands := make(map[string]pricingStand)
	blocks := make(map[string]pricingBlock)
	for _, slot := range slots {
		capacity, err := source.ActiveStands(ctx, tx, eventID, slot.ID)
		if err != nil {
			return nil, err
		}
		for _, stand := range capacity {
			option, exists := stands[stand.ID]
			if !exists {
				option = pricingStand{ID: stand.ID, Code: existing(nilIfEmpty(stand.Code), stand.ID), BlockID: stand.BlockID}
			}
			option.SlotIDs = append(option.SlotIDs, slot.ID)
			stands[stand.ID] = option
			if stand.BlockID != nil {
				blocks[*stand.BlockID] = pricingBlock{ID: *stand.BlockID, Label: existing(nilIfEmpty(stand.BlockLabel), *stand.BlockID)}
			}
		}
	}
	result := &pricingInventory{Stands: make([]pricingStand, 0, len(stands)), Blocks: make([]pricingBlock, 0, len(blocks))}
	for _, stand := range stands {
		result.Stands = append(result.Stands, stand)
	}
	for _, block := range blocks {
		result.Blocks = append(result.Blocks, block)
	}
	slices.SortFunc(result.Stands, func(a, b pricingStand) int { return cmp.Or(cmp.Compare(a.Code, b.Code), cmp.Compare(a.ID, b.ID)) })
	slices.SortFunc(result.Blocks, func(a, b pricingBlock) int { return cmp.Or(cmp.Compare(a.Label, b.Label), cmp.Compare(a.ID, b.ID)) })
	return result, nil
}
