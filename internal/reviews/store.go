// Package reviews menangani ulasan tenant dan penyelenggara setelah acara selesai.
package reviews

import (
	"context"
	"encoding/json"
	"errors"
	"kitakarsa.com/tenantku/backend/internal/platform/adminperm"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// editWindow: ulasan masih bisa diperbaiki penulisnya selama 30 hari setelah dibuat.
	editWindow  = 30 * 24 * time.Hour
	publicLimit = 20
	adminLimit  = 100
)

var (
	errNotFound  = errors.New("review not found")
	errForbidden = errors.New("admin access required")
	errExists    = errors.New("review already exists")
	errLocked    = errors.New("review can no longer be edited")
	errResolved  = errors.New("review already in requested state")
)

// notEligible membawa alasan booking belum boleh diulas.
type notEligible struct{ reason string }

func (e notEligible) Error() string { return "booking not eligible for review: " + e.reason }

type Store struct {
	DB *pgxpool.Pool
}

func (s Store) Ready(ctx context.Context) error {
	_, err := s.DB.Exec(ctx, `SELECT 1 FROM reviews, review_revisions LIMIT 0`)
	return err
}

// eligibility: booking confirmed, acara tidak dibatalkan, dan acara sudah berakhir atau berstatus selesai.
func eligibility(bookingStatus, eventStatus string, eventEnds, now time.Time) string {
	switch {
	case eventStatus == "cancelled":
		return "event_cancelled"
	case bookingStatus != "confirmed":
		return "booking_not_confirmed"
	case eventStatus != "completed" && eventEnds.After(now):
		return "event_not_finished"
	default:
		return ""
	}
}

type Review struct {
	ID         string    `json:"id"`
	BookingID  string    `json:"booking_id"`
	AuthorSide string    `json:"author_side"`
	AuthorName string    `json:"author_name"`
	EventName  string    `json:"event_name"`
	Rating     int       `json:"rating"`
	Comment    *string   `json:"comment,omitempty"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	Edited     bool      `json:"edited"`
	// EditableUntil hanya diisi untuk ulasan milik pihak pemanggil.
	EditableUntil *time.Time `json:"editable_until,omitempty"`
}

const reviewSelect = `SELECT r.id::text, r.booking_id::text, r.author_side,
	CASE r.author_side WHEN 'business' THEN z.name ELSE o.name END, e.name, r.rating, r.comment, r.status,
	r.created_at, r.updated_at, r.edit_count > 0
	FROM reviews r JOIN events e ON e.id = r.event_id
	JOIN businesses z ON z.id = r.business_id JOIN organizations o ON o.id = r.organization_id`

func scanReview(row pgx.Row) (Review, error) {
	var value Review
	err := row.Scan(&value.ID, &value.BookingID, &value.AuthorSide, &value.AuthorName, &value.EventName, &value.Rating,
		&value.Comment, &value.Status, &value.CreatedAt, &value.UpdatedAt, &value.Edited)
	return value, err
}

type bookingContext struct {
	event, eventName, business, organization, side string
	bookingStatus, eventStatus                     string
	eventEnds, now                                 time.Time
}

// loadBooking mengembalikan konteks booking dan pihak pemanggil. Bukan peserta booking: tidak ditemukan.
// Akun yang menjadi anggota kedua pihak diperlakukan sebagai tenant.
func loadBooking(ctx context.Context, tx pgx.Tx, user, booking string) (bookingContext, error) {
	var value bookingContext
	var side *string
	err := tx.QueryRow(ctx, `SELECT b.event_id::text, e.name, b.business_id::text, e.organization_id::text, b.status, e.status,
		e.ends_at, transaction_timestamp(), CASE
			WHEN EXISTS (SELECT 1 FROM memberships m WHERE m.user_id = $2 AND m.business_id = b.business_id) THEN 'business'
			WHEN EXISTS (SELECT 1 FROM memberships m WHERE m.user_id = $2 AND m.organization_id = e.organization_id) THEN 'organization'
		END
		FROM bookings b JOIN events e ON e.id = b.event_id WHERE b.id = $1`, booking, user).
		Scan(&value.event, &value.eventName, &value.business, &value.organization, &value.bookingStatus, &value.eventStatus,
			&value.eventEnds, &value.now, &side)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && side == nil) {
		return value, errNotFound
	}
	if err != nil {
		return value, err
	}
	value.side = *side
	return value, nil
}

// BookingReviews adalah keadaan ulasan satu booking dari sudut pandang pemanggil.
type BookingReviews struct {
	Side        string  `json:"side"`
	EventName   string  `json:"event_name"`
	Eligible    bool    `json:"eligible"`
	Reason      *string `json:"reason,omitempty"`
	Mine        *Review `json:"mine,omitempty"`
	Counterpart *Review `json:"counterpart,omitempty"`
}

func (s Store) forBooking(ctx context.Context, user, booking string) (BookingReviews, error) {
	var result BookingReviews
	err := pgx.BeginTxFunc(ctx, s.DB, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		info, err := loadBooking(ctx, tx, user, booking)
		if err != nil {
			return err
		}
		result.Side = info.side
		result.EventName = info.eventName
		if reason := eligibility(info.bookingStatus, info.eventStatus, info.eventEnds, info.now); reason != "" {
			result.Reason = &reason
		} else {
			result.Eligible = true
		}
		rows, err := tx.Query(ctx, reviewSelect+` WHERE r.booking_id = $1`, booking)
		if err != nil {
			return err
		}
		list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Review, error) { return scanReview(row) })
		if err != nil {
			return err
		}
		for i := range list {
			review := list[i]
			if review.AuthorSide == info.side {
				until := review.CreatedAt.Add(editWindow)
				review.EditableUntil = &until
				result.Mine = &review
			} else if review.Status == "published" {
				result.Counterpart = &review
			}
		}
		return nil
	})
	return result, err
}

type reviewInput struct {
	Rating  int     `json:"rating"`
	Comment *string `json:"comment"`
}

// create menulis ulasan pihak pemanggil. Ulasan kedua dari pihak yang sama ditolak.
func (s Store) create(ctx context.Context, user, booking string, input reviewInput) (string, error) {
	var id string
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		info, err := loadBooking(ctx, tx, user, booking)
		if err != nil {
			return err
		}
		if reason := eligibility(info.bookingStatus, info.eventStatus, info.eventEnds, info.now); reason != "" {
			return notEligible{reason: reason}
		}
		err = tx.QueryRow(ctx, `INSERT INTO reviews (booking_id, event_id, business_id, organization_id, author_side, author_id,
			rating, comment) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (booking_id, author_side) DO NOTHING RETURNING id::text`,
			booking, info.event, info.business, info.organization, info.side, user, input.Rating, input.Comment).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return errExists
		}
		return err
	})
	return id, err
}

// update menyimpan versi lama ke review_revisions lalu menulis versi baru. Hanya anggota pihak penulis.
func (s Store) update(ctx context.Context, user, id string, input reviewInput) (Review, error) {
	var result Review
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var side, status string
		var rating int
		var comment *string
		var created time.Time
		var allowed bool
		err := tx.QueryRow(ctx, `SELECT r.author_side, r.status, r.rating, r.comment, r.created_at,
			EXISTS (SELECT 1 FROM memberships m WHERE m.user_id = $2 AND
				CASE r.author_side WHEN 'business' THEN m.business_id = r.business_id ELSE m.organization_id = r.organization_id END)
			FROM reviews r WHERE r.id = $1 FOR UPDATE`, id, user).Scan(&side, &status, &rating, &comment, &created, &allowed)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !allowed) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		var now time.Time
		if err := tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&now); err != nil {
			return err
		}
		if status != "published" || now.After(created.Add(editWindow)) {
			return errLocked
		}
		if rating != input.Rating || !sameText(comment, input.Comment) {
			if _, err := tx.Exec(ctx, `INSERT INTO review_revisions (review_id, rating, comment, edited_by) VALUES ($1, $2, $3, $4)`,
				id, rating, comment, user); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE reviews SET rating = $2, comment = $3, updated_at = clock_timestamp(),
				edit_count = edit_count + 1 WHERE id = $1`, id, input.Rating, input.Comment); err != nil {
				return err
			}
		}
		result, err = scanReview(tx.QueryRow(ctx, reviewSelect+` WHERE r.id = $1`, id))
		if err == nil {
			until := result.CreatedAt.Add(editWindow)
			result.EditableUntil = &until
		}
		return err
	})
	return result, err
}

func sameText(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

type PublicReview struct {
	ID           string    `json:"id"`
	Rating       int       `json:"rating"`
	Comment      *string   `json:"comment,omitempty"`
	BusinessName string    `json:"business_name"`
	EventName    string    `json:"event_name"`
	CreatedAt    time.Time `json:"created_at"`
	Edited       bool      `json:"edited"`
}

type OrganizerReviews struct {
	OrganizationID   string         `json:"organization_id"`
	OrganizationName string         `json:"organization_name"`
	Count            int            `json:"count"`
	Average          *float64       `json:"average,omitempty"`
	Reviews          []PublicReview `json:"reviews"`
}

// organizer: ulasan tenant yang tayang untuk penyelenggara, ringkasan rata-rata, dan 20 ulasan terbaru.
func (s Store) organizer(ctx context.Context, organization string) (OrganizerReviews, error) {
	result := OrganizerReviews{Reviews: []PublicReview{}}
	err := pgx.BeginTxFunc(ctx, s.DB, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT o.id::text, o.name, count(r.id), round(avg(r.rating), 1)::float8 FROM organizations o
			LEFT JOIN reviews r ON r.organization_id = o.id AND r.author_side = 'business' AND r.status = 'published'
			WHERE o.id = $1 GROUP BY o.id`, organization).
			Scan(&result.OrganizationID, &result.OrganizationName, &result.Count, &result.Average)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT r.id::text, r.rating, r.comment, z.name, e.name, r.created_at, r.edit_count > 0
			FROM reviews r JOIN businesses z ON z.id = r.business_id JOIN events e ON e.id = r.event_id
			WHERE r.organization_id = $1 AND r.author_side = 'business' AND r.status = 'published'
			ORDER BY r.created_at DESC, r.id DESC LIMIT $2`, organization, publicLimit)
		if err != nil {
			return err
		}
		result.Reviews, err = pgx.CollectRows(rows, pgx.RowToStructByPos[PublicReview])
		if result.Reviews == nil {
			result.Reviews = []PublicReview{}
		}
		return err
	})
	return result, err
}

type AdminReview struct {
	Review
	AuthorEmail      string `json:"author_email"`
	BusinessName     string `json:"business_name"`
	OrganizationName string `json:"organization_name"`
	Revisions        int    `json:"revisions"`
}

func requireAdmin(ctx context.Context, tx pgx.Tx, actor string) error {
	allowed, err := adminperm.Has(ctx, tx, actor, adminperm.ModerationManage)
	if err != nil {
		return err
	}
	if !allowed {
		return errForbidden
	}
	return nil
}

const adminSelect = `SELECT r.id::text, r.booking_id::text, r.author_side,
	CASE r.author_side WHEN 'business' THEN z.name ELSE o.name END, e.name, r.rating, r.comment, r.status,
	r.created_at, r.updated_at, r.edit_count > 0, u.email, z.name, o.name,
	(SELECT count(*) FROM review_revisions v WHERE v.review_id = r.id)
	FROM reviews r JOIN events e ON e.id = r.event_id JOIN users u ON u.id = r.author_id
	JOIN businesses z ON z.id = r.business_id JOIN organizations o ON o.id = r.organization_id`

func scanAdmin(row pgx.Row) (AdminReview, error) {
	var value AdminReview
	err := row.Scan(&value.ID, &value.BookingID, &value.AuthorSide, &value.AuthorName, &value.EventName, &value.Rating,
		&value.Comment, &value.Status, &value.CreatedAt, &value.UpdatedAt, &value.Edited, &value.AuthorEmail,
		&value.BusinessName, &value.OrganizationName, &value.Revisions)
	return value, err
}

func (s Store) adminList(ctx context.Context, actor, status string) ([]AdminReview, error) {
	var result []AdminReview
	err := pgx.BeginTxFunc(ctx, s.DB, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if err := requireAdmin(ctx, tx, actor); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, adminSelect+` WHERE r.status = $1 ORDER BY r.updated_at DESC, r.id DESC LIMIT $2`, status, adminLimit)
		if err != nil {
			return err
		}
		result, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (AdminReview, error) { return scanAdmin(row) })
		return err
	})
	return result, err
}

// moderate menyembunyikan atau memulihkan ulasan dan mencatat keputusan di admin_audit.
func (s Store) moderate(ctx context.Context, actor, id, action, reason, requestID string) (AdminReview, error) {
	var result AdminReview
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if err := requireAdmin(ctx, tx, actor); err != nil {
			return err
		}
		var current string
		err := tx.QueryRow(ctx, `SELECT status FROM reviews WHERE id = $1 FOR UPDATE`, id).Scan(&current)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		next := "hidden"
		if action == "restore" {
			next = "published"
		}
		if current == next {
			return errResolved
		}
		if _, err := tx.Exec(ctx, `UPDATE reviews SET status = $2 WHERE id = $1`, id, next); err != nil {
			return err
		}
		before, err := json.Marshal(map[string]string{"status": current})
		if err != nil {
			return err
		}
		after, err := json.Marshal(map[string]string{"status": next})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO admin_audit (actor_id, action, resource_type, resource_id, reason, before, after, request_id)
			VALUES ($1, $2, 'review', $3, $4, $5, $6, $7)`, actor, "review."+action, id, reason, before, after, requestID); err != nil {
			return err
		}
		result, err = scanAdmin(tx.QueryRow(ctx, adminSelect+` WHERE r.id = $1`, id))
		return err
	})
	return result, err
}
