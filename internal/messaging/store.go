// Package messaging menangani chat tenant–penyelenggara, pelaporan pesan, dan moderasi admin.
package messaging

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// Batas kirim per akun di semua percakapan, agar satu akun tidak membanjiri pihak lain.
	rateWindow   = time.Minute
	rateLimit    = 30
	listLimit    = 100
	hiddenNotice = "Pesan ini disembunyikan moderator."
)

var (
	errNotFound            = errors.New("conversation not found")
	errForbidden           = errors.New("admin access required")
	errRateLimited         = errors.New("message rate limited")
	errIdempotencyMismatch = errors.New("client_message_id reused with different text")
	errOwnMessage          = errors.New("cannot report own message")
	errResolved            = errors.New("report already resolved")
)

type Store struct {
	DB *pgxpool.Pool
}

func (s Store) Ready(ctx context.Context) error {
	_, err := s.DB.Exec(ctx, `SELECT 1 FROM conversations, messages, conversation_reads, message_reports LIMIT 0`)
	return err
}

type LastMessage struct {
	Text       string    `json:"text"`
	SenderSide string    `json:"sender_side"`
	CreatedAt  time.Time `json:"created_at"`
	Hidden     bool      `json:"hidden,omitempty"`
}

type Conversation struct {
	ID               string       `json:"id"`
	EventID          string       `json:"event_id"`
	EventName        string       `json:"event_name"`
	BusinessID       string       `json:"business_id"`
	BusinessName     string       `json:"business_name"`
	OrganizationID   string       `json:"organization_id"`
	OrganizationName string       `json:"organization_name"`
	BookingID        string       `json:"booking_id"`
	Side             string       `json:"side"`
	CreatedAt        time.Time    `json:"created_at"`
	LastMessage      *LastMessage `json:"last_message,omitempty"`
	UnreadCount      int          `json:"unread_count"`
}

type Message struct {
	ID              string    `json:"id"`
	ClientMessageID string    `json:"client_message_id"`
	SenderID        string    `json:"sender_id"`
	SenderSide      string    `json:"sender_side"`
	Text            string    `json:"text"`
	CreatedAt       time.Time `json:"created_at"`
	Hidden          bool      `json:"hidden,omitempty"`
}

// sideExpr menentukan pihak akun $1 pada percakapan c; NULL bila akun bukan peserta.
const sideExpr = `CASE
	WHEN EXISTS (SELECT 1 FROM memberships m WHERE m.user_id = $1 AND m.business_id = c.business_id) THEN 'business'
	WHEN EXISTS (SELECT 1 FROM memberships m WHERE m.user_id = $1 AND m.organization_id = c.organization_id) THEN 'organization'
	END`

const conversationSelect = `SELECT c.id::text, c.event_id::text, e.name, c.business_id::text, z.name, c.organization_id::text,
	o.name, c.booking_id::text, ` + sideExpr + `, c.created_at, lm.body, lm.sender_side, lm.created_at, lm.hidden,
	(SELECT count(*) FROM messages u WHERE u.conversation_id = c.id AND u.sender_id <> $1
		AND u.created_at > COALESCE(r.last_read_at, '-infinity'))
	FROM conversations c
	JOIN events e ON e.id = c.event_id
	JOIN businesses z ON z.id = c.business_id
	JOIN organizations o ON o.id = c.organization_id
	LEFT JOIN conversation_reads r ON r.conversation_id = c.id AND r.user_id = $1
	LEFT JOIN LATERAL (SELECT m.body, m.sender_side, m.created_at, m.hidden_at IS NOT NULL AS hidden FROM messages m
		WHERE m.conversation_id = c.id ORDER BY m.created_at DESC, m.id DESC LIMIT 1) lm ON true`

func scanConversation(row pgx.Row) (Conversation, error) {
	var value Conversation
	var side, text, senderSide *string
	var at *time.Time
	var hidden *bool
	if err := row.Scan(&value.ID, &value.EventID, &value.EventName, &value.BusinessID, &value.BusinessName,
		&value.OrganizationID, &value.OrganizationName, &value.BookingID, &side, &value.CreatedAt,
		&text, &senderSide, &at, &hidden, &value.UnreadCount); err != nil {
		return value, err
	}
	if side == nil {
		return value, errNotFound
	}
	value.Side = *side
	if text != nil && senderSide != nil && at != nil {
		last := LastMessage{Text: *text, SenderSide: *senderSide, CreatedAt: *at, Hidden: hidden != nil && *hidden}
		if last.Hidden {
			last.Text = hiddenNotice
		}
		value.LastMessage = &last
	}
	return value, nil
}

// list mengembalikan percakapan yang diikuti akun, aktivitas terbaru dulu.
func (s Store) list(ctx context.Context, user string) ([]Conversation, error) {
	rows, err := s.DB.Query(ctx, conversationSelect+`
		WHERE `+sideExpr+` IS NOT NULL
		ORDER BY COALESCE(c.last_message_at, c.created_at) DESC, c.id DESC LIMIT $2`, user, listLimit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Conversation, error) { return scanConversation(row) })
}

func (s Store) get(ctx context.Context, user, id string) (Conversation, error) {
	value, err := scanConversation(s.DB.QueryRow(ctx, conversationSelect+` WHERE c.id = $2`, user, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return value, errNotFound
	}
	return value, err
}

// openForBooking membuka (atau mengembalikan) percakapan acara dan usaha dari booking.
// Hanya anggota usaha pemesan atau anggota organisasi pemilik acara yang boleh.
func (s Store) openForBooking(ctx context.Context, user, booking string) (Conversation, bool, error) {
	var event, business, organization string
	var allowed bool
	err := s.DB.QueryRow(ctx, `SELECT b.event_id::text, b.business_id::text, e.organization_id::text,
		EXISTS (SELECT 1 FROM memberships m WHERE m.user_id = $2
			AND (m.business_id = b.business_id OR m.organization_id = e.organization_id))
		FROM bookings b JOIN events e ON e.id = b.event_id WHERE b.id = $1`, booking, user).
		Scan(&event, &business, &organization, &allowed)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !allowed) {
		return Conversation{}, false, errNotFound
	}
	if err != nil {
		return Conversation{}, false, err
	}
	var id string
	created := true
	err = s.DB.QueryRow(ctx, `INSERT INTO conversations (event_id, business_id, organization_id, booking_id, created_by)
		VALUES ($1, $2, $3, $4, $5) ON CONFLICT (event_id, business_id) DO NOTHING RETURNING id::text`,
		event, business, organization, booking, user).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		created = false
		err = s.DB.QueryRow(ctx, `SELECT id::text FROM conversations WHERE event_id = $1 AND business_id = $2`,
			event, business).Scan(&id)
	}
	if err != nil {
		return Conversation{}, false, err
	}
	value, err := s.get(ctx, user, id)
	return value, created, err
}

func participantSide(ctx context.Context, q pgx.Tx, user, conversation string) (string, error) {
	var side *string
	err := q.QueryRow(ctx, `SELECT `+sideExpr+` FROM conversations c WHERE c.id = $2`, user, conversation).Scan(&side)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && side == nil) {
		return "", errNotFound
	}
	if err != nil {
		return "", err
	}
	return *side, nil
}

const messageColumns = `id::text, client_message_id::text, sender_id::text, sender_side, body, created_at, hidden_at IS NOT NULL`

func scanMessage(row pgx.Row) (Message, error) {
	var value Message
	err := row.Scan(&value.ID, &value.ClientMessageID, &value.SenderID, &value.SenderSide, &value.Text, &value.CreatedAt, &value.Hidden)
	if value.Hidden {
		value.Text = hiddenNotice
	}
	return value, err
}

// messages: terbaru dulu dengan cursor (created_at, id). Klien membalik urutan untuk ditampilkan.
func (s Store) messages(ctx context.Context, user, conversation string, before time.Time, beforeID string, limit int) ([]Message, error) {
	var result []Message
	err := pgx.BeginTxFunc(ctx, s.DB, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if _, err := participantSide(ctx, tx, user, conversation); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+messageColumns+` FROM messages
			WHERE conversation_id = $1 AND ($2 = '' OR (created_at, id) < ($3, $2::uuid))
			ORDER BY created_at DESC, id DESC LIMIT $4`, conversation, beforeID, before, limit)
		if err != nil {
			return err
		}
		result, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Message, error) { return scanMessage(row) })
		return err
	})
	return result, err
}

// send menyimpan pesan sekali per client_message_id. Pengiriman ulang dengan teks sama mengembalikan pesan
// yang sudah ada (created=false); teks berbeda ditolak.
func (s Store) send(ctx context.Context, user, conversation, clientID, text string) (Message, bool, error) {
	var result Message
	created := false
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		side, err := participantSide(ctx, tx, user, conversation)
		if err != nil {
			return err
		}
		existing, err := scanMessage(tx.QueryRow(ctx, `SELECT `+messageColumns+` FROM messages
			WHERE conversation_id = $1 AND sender_id = $2 AND client_message_id = $3`, conversation, user, clientID))
		if err == nil {
			result = existing
			return replayCheck(ctx, tx, existing.ID, text)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var recent int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM messages WHERE sender_id = $1
			AND created_at > clock_timestamp() - make_interval(secs => $2)`, user, rateWindow.Seconds()).Scan(&recent); err != nil {
			return err
		}
		if recent >= rateLimit {
			return errRateLimited
		}
		result, err = scanMessage(tx.QueryRow(ctx, `INSERT INTO messages (conversation_id, sender_id, sender_side, client_message_id, body)
			VALUES ($1, $2, $3, $4, $5) ON CONFLICT (conversation_id, sender_id, client_message_id) DO NOTHING
			RETURNING `+messageColumns, conversation, user, side, clientID, text))
		if errors.Is(err, pgx.ErrNoRows) {
			// Permintaan paralel dengan client_message_id yang sama sudah lebih dulu tersimpan.
			result, err = scanMessage(tx.QueryRow(ctx, `SELECT `+messageColumns+` FROM messages
				WHERE conversation_id = $1 AND sender_id = $2 AND client_message_id = $3`, conversation, user, clientID))
			if err != nil {
				return err
			}
			return replayCheck(ctx, tx, result.ID, text)
		}
		if err != nil {
			return err
		}
		created = true
		if _, err := tx.Exec(ctx, `UPDATE conversations SET last_message_at = $2
			WHERE id = $1 AND (last_message_at IS NULL OR last_message_at < $2)`, conversation, result.CreatedAt); err != nil {
			return err
		}
		return markReadAt(ctx, tx, user, conversation, result.CreatedAt)
	})
	return result, created, err
}

func replayCheck(ctx context.Context, tx pgx.Tx, id, text string) error {
	var same bool
	if err := tx.QueryRow(ctx, `SELECT body = $2 FROM messages WHERE id = $1`, id, text).Scan(&same); err != nil {
		return err
	}
	if !same {
		return errIdempotencyMismatch
	}
	return nil
}

func markReadAt(ctx context.Context, tx pgx.Tx, user, conversation string, at time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO conversation_reads (conversation_id, user_id, last_read_at) VALUES ($1, $2, $3)
		ON CONFLICT (conversation_id, user_id) DO UPDATE
		SET last_read_at = GREATEST(conversation_reads.last_read_at, EXCLUDED.last_read_at)`, conversation, user, at)
	return err
}

func (s Store) markRead(ctx context.Context, user, conversation string) error {
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if _, err := participantSide(ctx, tx, user, conversation); err != nil {
			return err
		}
		var now time.Time
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
			return err
		}
		return markReadAt(ctx, tx, user, conversation, now)
	})
}

// unreadTotal menjumlahkan pesan dari pihak lain yang belum dibaca di semua percakapan akun.
func (s Store) unreadTotal(ctx context.Context, user string) (int, error) {
	var total int
	err := s.DB.QueryRow(ctx, `SELECT count(*) FROM messages u
		JOIN conversations c ON c.id = u.conversation_id
		LEFT JOIN conversation_reads r ON r.conversation_id = c.id AND r.user_id = $1
		WHERE u.sender_id <> $1 AND u.created_at > COALESCE(r.last_read_at, '-infinity')
		AND `+sideExpr+` IS NOT NULL`, user).Scan(&total)
	return total, err
}

type Report struct {
	ID        string    `json:"id"`
	MessageID string    `json:"message_id"`
	Reason    string    `json:"reason"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

// report mencatat laporan satu kali per pelapor dan pesan. Pesan sendiri tidak bisa dilaporkan.
func (s Store) report(ctx context.Context, user, message, reason string, note *string) (Report, bool, error) {
	var result Report
	created := false
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var conversation, sender string
		err := tx.QueryRow(ctx, `SELECT conversation_id::text, sender_id::text FROM messages WHERE id = $1`, message).
			Scan(&conversation, &sender)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		if _, err := participantSide(ctx, tx, user, conversation); err != nil {
			return err
		}
		if sender == user {
			return errOwnMessage
		}
		const columns = `id::text, message_id::text, reason, status, created_at`
		err = tx.QueryRow(ctx, `INSERT INTO message_reports (message_id, reporter_id, reason, note) VALUES ($1, $2, $3, $4)
			ON CONFLICT (message_id, reporter_id) DO NOTHING RETURNING `+columns, message, user, reason, note).
			Scan(&result.ID, &result.MessageID, &result.Reason, &result.Status, &result.CreatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return tx.QueryRow(ctx, `SELECT `+columns+` FROM message_reports WHERE message_id = $1 AND reporter_id = $2`,
				message, user).Scan(&result.ID, &result.MessageID, &result.Reason, &result.Status, &result.CreatedAt)
		}
		created = err == nil
		return err
	})
	return result, created, err
}
