package notify

import (
	"context"
	"sync"
	"testing"
	"time"

	"kitakarsa.com/tenantku/backend/internal/platform/testdb"
)

// E15-06: satu peristiwa menghasilkan satu notifikasi per penerima walau worker berjalan berulang dan paralel.
func TestNotificationDedupeIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool := testdb.Migrated(t, ctx, "notify_test")
	s := Store{DB: pool}
	w := testdb.NewWorld(t, ctx, pool, "cancelled", false)
	testdb.Exec(t, ctx, pool, `INSERT INTO booking_status_history (booking_id, from_status, to_status, reason)
		VALUES ($1, 'held', 'cancelled', 'event_cancelled')`, w.Booking)

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Collect(ctx); err != nil {
				t.Errorf("collect: %v", err)
			}
			if _, _, err := s.Deliver(ctx, 10); err != nil {
				t.Errorf("deliver: %v", err)
			}
		}()
	}
	wg.Wait()
	if _, err := s.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Deliver(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if got := testdb.Scalar(t, ctx, pool, `SELECT count(*) FROM notification_outbox`); got != "1" {
		t.Fatalf("outbox rows = %s, want 1", got)
	}
	if got := testdb.Scalar(t, ctx, pool, `SELECT count(*) FROM notifications WHERE user_id = $1`, w.Tenant); got != "1" {
		t.Fatalf("tenant notifications = %s, want 1", got)
	}
	if got := testdb.Scalar(t, ctx, pool, `SELECT count(*) FROM notifications WHERE user_id <> $1`, w.Tenant); got != "0" {
		t.Fatalf("notification leaked to %s other users", got)
	}
	if count, err := s.unreadCount(ctx, w.Tenant); err != nil || count != 1 {
		t.Fatalf("unread = %d %v", count, err)
	}
}
