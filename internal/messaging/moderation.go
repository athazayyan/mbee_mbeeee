package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// ModerationReport adalah laporan pesan untuk antrean admin, lengkap dengan isi asli pesan.
type ModerationReport struct {
	ID            string     `json:"id"`
	Status        string     `json:"status"`
	Reason        string     `json:"reason"`
	Note          *string    `json:"note,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	ResolvedAt    *time.Time `json:"resolved_at,omitempty"`
	ReporterEmail string     `json:"reporter_email"`
	MessageID     string     `json:"message_id"`
	MessageText   string     `json:"message_text"`
	MessageHidden bool       `json:"message_hidden"`
	SenderEmail   string     `json:"sender_email"`
	SenderSide    string     `json:"sender_side"`
	EventName     string     `json:"event_name"`
	BusinessName  string     `json:"business_name"`
	Organization  string     `json:"organization_name"`
	SentAt        time.Time  `json:"sent_at"`
}

func requireAdmin(ctx context.Context, tx pgx.Tx, actor string) error {
	var admin bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM platform_admins WHERE user_id = $1)`, actor).Scan(&admin); err != nil {
		return err
	}
	if !admin {
		return errForbidden
	}
	return nil
}

const reportSelect = `SELECT r.id::text, r.status, r.reason, r.note, r.created_at, r.resolved_at, ru.email,
	m.id::text, m.body, m.hidden_at IS NOT NULL, su.email, m.sender_side, e.name, z.name, o.name, m.created_at
	FROM message_reports r
	JOIN users ru ON ru.id = r.reporter_id
	JOIN messages m ON m.id = r.message_id
	JOIN users su ON su.id = m.sender_id
	JOIN conversations c ON c.id = m.conversation_id
	JOIN events e ON e.id = c.event_id
	JOIN businesses z ON z.id = c.business_id
	JOIN organizations o ON o.id = c.organization_id`

func scanReport(row pgx.Row) (ModerationReport, error) {
	var value ModerationReport
	err := row.Scan(&value.ID, &value.Status, &value.Reason, &value.Note, &value.CreatedAt, &value.ResolvedAt,
		&value.ReporterEmail, &value.MessageID, &value.MessageText, &value.MessageHidden, &value.SenderEmail,
		&value.SenderSide, &value.EventName, &value.BusinessName, &value.Organization, &value.SentAt)
	return value, err
}

// reports: status kosong berarti laporan terbuka, terlama dulu agar antrean diproses berurutan.
func (s Store) reports(ctx context.Context, actor, status string) ([]ModerationReport, error) {
	var result []ModerationReport
	err := pgx.BeginTxFunc(ctx, s.DB, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if err := requireAdmin(ctx, tx, actor); err != nil {
			return err
		}
		order := "r.created_at, r.id"
		if status != "open" {
			order = "r.resolved_at DESC, r.id DESC"
		}
		rows, err := tx.Query(ctx, reportSelect+` WHERE r.status = $1 ORDER BY `+order+` LIMIT $2`, status, listLimit)
		if err != nil {
			return err
		}
		result, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (ModerationReport, error) { return scanReport(row) })
		return err
	})
	return result, err
}

// resolve menindaklanjuti laporan. "hide" menyembunyikan pesan dan menutup semua laporan terbuka untuk pesan
// itu; "dismiss" hanya menutup laporan ini. Setiap keputusan tercatat di admin_audit.
func (s Store) resolve(ctx context.Context, actor, id, action, reason, requestID string) (ModerationReport, error) {
	var result ModerationReport
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if err := requireAdmin(ctx, tx, actor); err != nil {
			return err
		}
		var status, message string
		err := tx.QueryRow(ctx, `SELECT status, message_id::text FROM message_reports WHERE id = $1 FOR UPDATE`, id).
			Scan(&status, &message)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		if status != "open" {
			return errResolved
		}
		auditAction, resourceType, resourceID := "message_report.dismiss", "message_report", id
		before, after := map[string]any{"status": "open"}, map[string]any{"status": "dismissed"}
		if action == "hide" {
			auditAction, resourceType, resourceID = "message.hide", "message", message
			if _, err := tx.Exec(ctx, `UPDATE messages SET hidden_at = clock_timestamp(), hidden_by = $2
				WHERE id = $1 AND hidden_at IS NULL`, message, actor); err != nil {
				return err
			}
			tag, err := tx.Exec(ctx, `UPDATE message_reports SET status = 'actioned', resolved_at = clock_timestamp(), resolved_by = $2
				WHERE message_id = $1 AND status = 'open'`, message, actor)
			if err != nil {
				return err
			}
			before = map[string]any{"hidden": false, "open_reports": tag.RowsAffected()}
			after = map[string]any{"hidden": true, "open_reports": 0, "report_id": id}
		} else if _, err := tx.Exec(ctx, `UPDATE message_reports SET status = 'dismissed', resolved_at = clock_timestamp(),
			resolved_by = $2 WHERE id = $1`, id, actor); err != nil {
			return err
		}
		beforeJSON, err := json.Marshal(before)
		if err != nil {
			return err
		}
		afterJSON, err := json.Marshal(after)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO admin_audit (actor_id, action, resource_type, resource_id, reason, before, after, request_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, actor, auditAction, resourceType, resourceID, reason, beforeJSON, afterJSON, requestID); err != nil {
			return err
		}
		result, err = scanReport(tx.QueryRow(ctx, reportSelect+` WHERE r.id = $1`, id))
		return err
	})
	return result, err
}
