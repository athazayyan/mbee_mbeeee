package events

import (
	"context"
	"testing"
	"time"

	"kitakarsa.com/tenantku/backend/internal/platform/testdb"
)

// Denah publik menampilkan usaha pada stand terkonfirmasi beserta produk aktifnya; booking yang belum lunas tetap anonim.
func TestPublicLayoutOccupantIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool := testdb.Migrated(t, ctx, "occupant_test")
	s := Store{DB: pool}
	w := testdb.NewWorld(t, ctx, pool, "confirmed", false)

	layout := validLayout()
	if fields := layout.validate(); fields != nil {
		t.Fatal(fields)
	}
	if _, err := s.saveLayout(ctx, w.Organizer, w.Event, "", layout); err != nil {
		t.Fatal(err)
	}
	slot := testdb.Scalar(t, ctx, pool, `INSERT INTO event_slots (event_id, code, starts_at, ends_at)
		SELECT id, 'whole-event', starts_at, ends_at FROM events WHERE id = $1 RETURNING id`, w.Event)
	testdb.Exec(t, ctx, pool, `INSERT INTO allocations (booking_id, event_id, event_slot_id, stand_id, block_id, status)
		VALUES ($1, $2, $3, $4, $5, 'reserved')`, w.Booking, w.Event, slot, standA, blockA)
	testdb.Exec(t, ctx, pool, `INSERT INTO products (business_id, name, price, created_by) VALUES ($1, 'Kopi Sanger', 15000, $2)`, w.Business, w.Tenant)
	testdb.Exec(t, ctx, pool, `INSERT INTO products (business_id, name, price, created_by, archived_at) VALUES ($1, 'Menu Lama', 9000, $2, now())`, w.Business, w.Tenant)

	public, err := s.publicLayoutFor(ctx, w.Event)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]publicStand{}
	for _, stand := range public.Stands {
		byID[stand.ID] = stand
	}
	occupied := byID[standA]
	if occupied.Status != "booked" || occupied.Occupant == nil {
		t.Fatalf("stand A = %+v, want booked with occupant", occupied)
	}
	if occupied.Occupant.BusinessName != "Kopi Senja" || occupied.Occupant.Category == "" {
		t.Fatalf("occupant = %+v", occupied.Occupant)
	}
	if len(occupied.Occupant.Products) != 1 || occupied.Occupant.Products[0].Name != "Kopi Sanger" {
		t.Fatalf("products = %+v, want only the active product", occupied.Occupant.Products)
	}
	if byID[standB].Occupant != nil {
		t.Fatalf("free stand must not carry an occupant: %+v", byID[standB])
	}

	testdb.Exec(t, ctx, pool, `UPDATE bookings SET status = 'payment_pending', expires_at = now() + interval '20 minutes' WHERE id = $1`, w.Booking)
	testdb.Exec(t, ctx, pool, `UPDATE allocations SET status = 'hold' WHERE booking_id = $1`, w.Booking)
	public, err = s.publicLayoutFor(ctx, w.Event)
	if err != nil {
		t.Fatal(err)
	}
	for _, stand := range public.Stands {
		if stand.Occupant != nil {
			t.Fatalf("pending booking must stay anonymous: %+v", stand)
		}
	}
}
