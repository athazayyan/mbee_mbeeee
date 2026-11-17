package events

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"kitakarsa.com/tenantku/backend/internal/platform/testdb"
)

// E05-05 (T10, T23 sisi server): edit denah bersamaan dengan revisi yang sama hanya satu yang tersimpan,
// dan stand yang sudah dipesan tidak bisa dihapus, dipindah, atau dinonaktifkan.
func TestLayoutConcurrencyIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool := testdb.Migrated(t, ctx, "layout_test")
	s := Store{DB: pool}
	w := testdb.NewWorld(t, ctx, pool, "confirmed", false)

	layout := validLayout()
	if fields := layout.validate(); fields != nil {
		t.Fatal(fields)
	}
	if _, err := s.saveLayout(ctx, w.Tenant, w.Event, "", layout); err == nil {
		t.Fatal("tenant must not edit the organizer layout")
	}
	first, err := s.saveLayout(ctx, w.Organizer, w.Event, "", layout)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.saveLayout(ctx, w.Organizer, w.Event, "", layout); !errors.Is(err, errLayoutPrecondition) {
		t.Fatalf("save without If-Match after first revision: %v", err)
	}

	// Dua tab mengirim perubahan dari revisi yang sama: satu menang, sisanya konflik.
	etag := layoutETag(first.Revision)
	var wg sync.WaitGroup
	results := make(chan error, 5)
	for i := range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			next := validLayout()
			next.Stands[1].X = 8 + i%2
			_ = next.validate()
			_, err := s.saveLayout(ctx, w.Organizer, w.Event, etag, next)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	saved, conflicts := 0, 0
	for err := range results {
		switch {
		case err == nil:
			saved++
		case errors.Is(err, errLayoutConflict):
			conflicts++
		default:
			t.Fatalf("concurrent save: %v", err)
		}
	}
	if saved != 1 || conflicts != 4 {
		t.Fatalf("saved=%d conflicts=%d, want 1 and 4", saved, conflicts)
	}
	revision := first.Revision + 1

	// Stand A dipesan: tidak boleh dihapus, dipindah, atau dinonaktifkan; stand lain tetap bisa diubah.
	slot := testdb.Scalar(t, ctx, pool, `INSERT INTO event_slots (event_id, code, starts_at, ends_at)
		SELECT id, 'whole-event', starts_at, ends_at FROM events WHERE id = $1 RETURNING id`, w.Event)
	testdb.Exec(t, ctx, pool, `INSERT INTO allocations (booking_id, event_id, event_slot_id, stand_id, block_id, status)
		VALUES ($1, $2, $3, $4, $5, 'reserved')`, w.Booking, w.Event, slot, standA, blockA)
	for name, change := range map[string]func(*layoutInput){
		"hapus":       func(l *layoutInput) { l.Stands = l.Stands[1:] },
		"pindah":      func(l *layoutInput) { l.Stands[0].Y = 2 },
		"nonaktifkan": func(l *layoutInput) { l.Stands[0].Active = false },
		"ganti kode":  func(l *layoutInput) { l.Stands[0].Code = "A-09" },
	} {
		next := validLayout()
		change(&next)
		_ = next.validate()
		var reserved reservedStandError
		if _, err := s.saveLayout(ctx, w.Organizer, w.Event, layoutETag(revision), next); !errors.As(err, &reserved) {
			t.Fatalf("%s reserved stand: %v", name, err)
		}
	}
	next := validLayout()
	next.Stands[1].Y = 4
	_ = next.validate()
	if _, err := s.saveLayout(ctx, w.Organizer, w.Event, layoutETag(revision), next); err != nil {
		t.Fatalf("moving an unreserved stand: %v", err)
	}
}
