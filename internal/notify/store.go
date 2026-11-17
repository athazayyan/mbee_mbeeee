// Package notify mengumpulkan peristiwa booking ke outbox lalu mengirimkannya ke inbox notifikasi akun.
package notify

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// collectWindow membatasi pemindaian tabel sumber; peristiwa lebih tua tidak dikirim lagi.
	collectWindow = 30 * 24 * time.Hour
	maxAttempts   = 8
	baseBackoff   = 30 * time.Second
	maxBackoff    = time.Hour
)

var errNotFound = errors.New("notification not found")

type Store struct {
	DB *pgxpool.Pool
}

// Notification adalah satu isi inbox milik akun.
type Notification struct {
	ID        string     `json:"id"`
	Kind      string     `json:"kind"`
	Title     string     `json:"title"`
	Message   string     `json:"message"`
	Link      *string    `json:"link,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	ReadAt    *time.Time `json:"read_at,omitempty"`
}

// Ready memeriksa inbox untuk API. Outbox hanya bisa dibaca worker.
func (s Store) Ready(ctx context.Context) error {
	_, err := s.DB.Exec(ctx, `SELECT 1 FROM notifications LIMIT 0`)
	return err
}

// WorkerReady memeriksa outbox dan inbox untuk proses worker.
func (s Store) WorkerReady(ctx context.Context) error {
	_, err := s.DB.Exec(ctx, `SELECT 1 FROM notification_outbox LIMIT 0`)
	if err == nil {
		_, err = s.DB.Exec(ctx, `SELECT id FROM notifications LIMIT 0`)
	}
	return err
}

// sources memetakan jenis notifikasi ke histori status booking yang memicunya.
// Referensi sumber adalah ID histori, sehingga satu peristiwa hanya masuk outbox sekali.
var sources = []struct{ kind, toStatus, reason string }{
	{kind: "event_cancelled", toStatus: "cancelled", reason: "event_cancelled"},
	{kind: "booking_expired", toStatus: "expired"},
}

// Collect menyalin peristiwa baru dari histori booking ke outbox. Aman dijalankan berulang dan paralel.
func (s Store) Collect(ctx context.Context) (int64, error) {
	since := time.Now().Add(-collectWindow)
	var total int64
	for _, source := range sources {
		tag, err := s.DB.Exec(ctx, `INSERT INTO notification_outbox (kind, source_ref, booking_id, event_id)
			SELECT $1, h.id::text, b.id, b.event_id FROM booking_status_history h
			JOIN bookings b ON b.id = h.booking_id
			WHERE h.to_status = $2 AND ($3 = '' OR h.reason = $3) AND h.created_at > $4
			ORDER BY h.id
			ON CONFLICT (kind, source_ref) DO NOTHING`, source.kind, source.toStatus, source.reason, since)
		if err != nil {
			return total, err
		}
		total += tag.RowsAffected()
	}
	return total, nil
}

type outboxItem struct {
	id        int64
	kind      string
	bookingID *string
	attempts  int
}

// Deliver mengirim paling banyak limit item outbox yang jatuh tempo. Item yang gagal dijadwalkan ulang
// dengan backoff tanpa menggagalkan item lain; setelah maxAttempts item ditandai failed.
func (s Store) Deliver(ctx context.Context, limit int) (delivered, failed int, err error) {
	err = pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, kind, booking_id::text, attempts FROM notification_outbox
			WHERE status = 'pending' AND next_attempt_at <= clock_timestamp()
			ORDER BY next_attempt_at, id LIMIT $1 FOR UPDATE SKIP LOCKED`, limit)
		if err != nil {
			return err
		}
		items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (outboxItem, error) {
			var item outboxItem
			return item, row.Scan(&item.id, &item.kind, &item.bookingID, &item.attempts)
		})
		if err != nil {
			return err
		}
		for _, item := range items {
			count, deliverErr := deliverOne(ctx, tx, item)
			if deliverErr == nil {
				if _, err := tx.Exec(ctx, `UPDATE notification_outbox SET status = 'delivered', attempts = attempts + 1,
					delivered_at = clock_timestamp(), recipients = $2, last_error = NULL WHERE id = $1`, item.id, count); err != nil {
					return err
				}
				delivered++
				continue
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			status := "pending"
			if item.attempts+1 >= maxAttempts {
				status = "failed"
				failed++
			}
			if _, err := tx.Exec(ctx, `UPDATE notification_outbox SET status = $2, attempts = attempts + 1,
				next_attempt_at = clock_timestamp() + make_interval(secs => $3), last_error = $4 WHERE id = $1`,
				item.id, status, backoff(item.attempts).Seconds(), errorCode(deliverErr)); err != nil {
				return err
			}
		}
		return nil
	})
	return delivered, failed, err
}

// deliverOne menulis notifikasi untuk semua penerima di dalam savepoint, sehingga kegagalan hanya
// membatalkan item ini.
func deliverOne(ctx context.Context, outer pgx.Tx, item outboxItem) (int, error) {
	if item.bookingID == nil {
		return 0, errors.New("outbox item without booking")
	}
	tx, err := outer.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var event, business, businessID string
	if err := tx.QueryRow(ctx, `SELECT e.name, z.name, z.id::text FROM bookings b
		JOIN events e ON e.id = b.event_id JOIN businesses z ON z.id = b.business_id
		WHERE b.id = $1`, *item.bookingID).Scan(&event, &business, &businessID); err != nil {
		return 0, err
	}
	title, body, err := render(item.kind, event, business)
	if err != nil {
		return 0, err
	}
	link := "/tenant/bookings/" + *item.bookingID
	tag, err := tx.Exec(ctx, `INSERT INTO notifications (user_id, outbox_id, kind, title, body, link)
		SELECT m.user_id, $2, $3, $4, $5, $6 FROM memberships m
		WHERE m.business_id = $1 AND m.role = 'owner'
		ON CONFLICT (user_id, outbox_id) DO NOTHING`, businessID, item.id, item.kind, title, body, link)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), tx.Commit(ctx)
}

// render menyusun judul dan isi notifikasi dari template per jenis.
func render(kind, event, business string) (string, string, error) {
	event, business = clip(event, 160), clip(business, 120)
	switch kind {
	case "event_cancelled":
		return "Acara dibatalkan",
			fmt.Sprintf("%s dibatalkan oleh penyelenggara. Pemesanan stand untuk %s ikut ditutup.", event, business), nil
	case "booking_expired":
		return "Hold stand berakhir",
			fmt.Sprintf("Hold stand di %s untuk %s berakhir karena pembayaran belum selesai. Pilih stand lagi bila masih tersedia.", event, business), nil
	default:
		return "", "", fmt.Errorf("unknown notification kind %q", kind)
	}
}

func clip(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit-1]) + "…"
}

// backoff: 30 detik, lalu dua kali lipat per percobaan, paling lama satu jam.
func backoff(attempts int) time.Duration {
	delay := baseBackoff
	for i := 0; i < attempts && delay < maxBackoff; i++ {
		delay *= 2
	}
	return min(delay, maxBackoff)
}

func errorCode(err error) string {
	var database *pgconn.PgError
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "source_missing"
	case errors.As(err, &database):
		return "db_" + database.Code
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	default:
		return "delivery_failed"
	}
}

// list mengembalikan inbox akun, terbaru dulu, dengan cursor (created_at, id).
func (s Store) list(ctx context.Context, user string, before time.Time, beforeID string, limit int, unread bool) ([]Notification, int, error) {
	var result []Notification
	var unreadCount int
	err := pgx.BeginTxFunc(ctx, s.DB, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id::text, kind, title, body, link, created_at, read_at FROM notifications
			WHERE user_id = $1 AND (NOT $2 OR read_at IS NULL)
			AND ($3 = '' OR (created_at, id) < ($4, $3::uuid))
			ORDER BY created_at DESC, id DESC LIMIT $5`, user, unread, beforeID, before, limit)
		if err != nil {
			return err
		}
		result, err = pgx.CollectRows(rows, pgx.RowToStructByPos[Notification])
		if err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE user_id = $1 AND read_at IS NULL`, user).Scan(&unreadCount)
	})
	return result, unreadCount, err
}

func (s Store) unreadCount(ctx context.Context, user string) (int, error) {
	var count int
	err := s.DB.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE user_id = $1 AND read_at IS NULL`, user).Scan(&count)
	return count, err
}

// markRead idempoten: notifikasi yang sudah dibaca tetap memakai waktu baca pertama.
func (s Store) markRead(ctx context.Context, user, id string) error {
	tag, err := s.DB.Exec(ctx, `UPDATE notifications SET read_at = COALESCE(read_at, clock_timestamp())
		WHERE id = $1 AND user_id = $2`, id, user)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errNotFound
	}
	return nil
}

func (s Store) markAllRead(ctx context.Context, user string) (int64, error) {
	tag, err := s.DB.Exec(ctx, `UPDATE notifications SET read_at = clock_timestamp()
		WHERE user_id = $1 AND read_at IS NULL`, user)
	return tag.RowsAffected(), err
}
