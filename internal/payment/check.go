package payment

import (
	"context"
	"fmt"
	"slices"
	"time"
)

// MethodCheck is the result of asking Midtrans to accept one method's channels.
type MethodCheck struct {
	Code     string
	Fee      string
	Channels []string
	// Missing lists channels Midtrans would not offer, usually because they are not activated.
	Missing []string
	Err     error
}

// CheckMethods opens a throwaway Snap token for every enabled method with only that method's channels
// and reads back which of them Midtrans will really offer. Midtrans accepts any channel name when a
// token is created, so only the read-back proves the channel is activated for the merchant account.
// Nothing is charged and nothing is stored locally: Snap creates a payable transaction only when someone
// picks a method on the hosted page.
func (s Store) CheckMethods(ctx context.Context) ([]MethodCheck, error) {
	if s.Provider == nil {
		return nil, ErrUnavailable
	}
	methods, err := enabledMethods(ctx, s.DB)
	if err != nil {
		return nil, err
	}
	const probe = int64(100000)
	var results []MethodCheck
	for _, m := range methods {
		fee, err := m.Fee(probe)
		if err != nil {
			return nil, err
		}
		var orderID string
		if err := s.DB.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&orderID); err != nil {
			return nil, err
		}
		snap, err := s.Provider.CreateSnap(ctx, SnapInput{
			OrderID: orderID, Amount: probe + fee, Channels: m.SnapChannels, Expiry: 5 * time.Minute,
			Items: probeItems(m.Code, probe, fee),
		})
		check := MethodCheck{Code: m.Code, Channels: m.SnapChannels, Err: err}
		if err == nil {
			offered, lookupErr := s.Provider.EnabledChannels(ctx, snap.Token)
			check.Err = lookupErr
			if lookupErr == nil {
				for _, channel := range m.SnapChannels {
					if !slices.Contains(offered, channel) {
						check.Missing = append(check.Missing, channel)
					}
				}
			}
		}
		check.Fee = fmt.Sprintf("Rp%d", fee)
		results = append(results, check)
	}
	return results, nil
}

func probeItems(code string, probe, fee int64) []snapItem {
	items := []snapItem{{ID: "probe", Name: "Pemeriksaan metode " + code, Price: probe, Quantity: 1}}
	if fee > 0 {
		items = append(items, snapItem{ID: "fee", Name: "Biaya admin", Price: fee, Quantity: 1})
	}
	return items
}
