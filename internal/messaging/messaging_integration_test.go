package messaging

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"kitakarsa.com/tenantku/backend/internal/platform/testdb"
)

// E15-06: percakapan terpisah antarorganisasi dan pengiriman ulang tanpa duplikasi.
func TestConversationIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool := testdb.Migrated(t, ctx, "messaging_test")
	s := Store{DB: pool}
	w := testdb.NewWorld(t, ctx, pool, "confirmed", false)
	other := testdb.NewWorld(t, ctx, pool, "confirmed", false)

	conversation, created, err := s.openForBooking(ctx, w.Tenant, w.Booking)
	if err != nil || !created || conversation.Side != "business" {
		t.Fatalf("open: %+v created=%v err=%v", conversation, created, err)
	}
	if again, created, err := s.openForBooking(ctx, w.Organizer, w.Booking); err != nil || created || again.ID != conversation.ID {
		t.Fatalf("organizer must reuse conversation: created=%v err=%v", created, err)
	}
	// Peserta acara lain (tenant dan penyelenggara) tidak bisa membuka, membaca, atau mengirim.
	for _, outsider := range []string{other.Tenant, other.Organizer} {
		if _, _, err := s.openForBooking(ctx, outsider, w.Booking); !errors.Is(err, errNotFound) {
			t.Fatalf("outsider opened conversation: %v", err)
		}
		if _, err := s.messages(ctx, outsider, conversation.ID, time.Time{}, "", 10); !errors.Is(err, errNotFound) {
			t.Fatalf("outsider read messages: %v", err)
		}
		if _, _, err := s.send(ctx, outsider, conversation.ID, "6f1f9d1e-2b7c-4d5e-9a3b-1c2d3e4f5a6b", "halo"); !errors.Is(err, errNotFound) {
			t.Fatalf("outsider sent message: %v", err)
		}
		if list, err := s.list(ctx, outsider); err != nil || len(list) != 0 {
			t.Fatalf("outsider list leaked: %d %v", len(list), err)
		}
	}

	// Kirim ulang paralel dengan client_message_id yang sama menghasilkan satu pesan.
	clientID := "0c1d2e3f-4a5b-4c6d-8e7f-8091a2b3c4d5"
	var wg sync.WaitGroup
	ids := make(chan string, 6)
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			message, _, err := s.send(ctx, w.Tenant, conversation.ID, clientID, "Apakah stand bisa dipindah?")
			if err != nil {
				t.Errorf("send: %v", err)
				return
			}
			ids <- message.ID
		}()
	}
	wg.Wait()
	close(ids)
	unique := map[string]bool{}
	for id := range ids {
		unique[id] = true
	}
	if len(unique) != 1 || testdb.Scalar(t, ctx, pool, `SELECT count(*) FROM messages WHERE client_message_id = $1`, clientID) != "1" {
		t.Fatalf("resend created %d messages", len(unique))
	}
	if _, _, err := s.send(ctx, w.Tenant, conversation.ID, clientID, "teks lain"); !errors.Is(err, errIdempotencyMismatch) {
		t.Fatalf("different text with same id: %v", err)
	}
	if count, err := s.unreadTotal(ctx, w.Organizer); err != nil || count != 1 {
		t.Fatalf("organizer unread = %d %v", count, err)
	}
	if count, err := s.unreadTotal(ctx, w.Tenant); err != nil || count != 0 {
		t.Fatalf("own message counted unread: %d %v", count, err)
	}
}
