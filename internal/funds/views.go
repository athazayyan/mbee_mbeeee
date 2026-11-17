package funds

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

// TimelineItem is one step in a booking's life, newest last.
type TimelineItem struct {
	At     time.Time `json:"at"`
	Kind   string    `json:"kind"`
	Title  string    `json:"title"`
	Detail string    `json:"detail,omitempty"`
}

// BookingFunds is the tenant's view of check-in, refund, and dispute for one booking.
type BookingFunds struct {
	BookingID  string         `json:"booking_id"`
	Status     string         `json:"status"`
	CheckedIn  *time.Time     `json:"checked_in_at"`
	Refunds    []Refund       `json:"refunds"`
	Dispute    *Dispute       `json:"dispute"`
	CanDispute bool           `json:"can_dispute"`
	Timeline   []TimelineItem `json:"timeline"`
}

// rupiah writes an amount the way the app shows it, with dots between thousands.
func rupiah(n int64) string {
	digits := strconv.FormatInt(n, 10)
	var out []byte
	for i := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			out = append(out, '.')
		}
		out = append(out, digits[i])
	}
	return "Rp" + string(out)
}

var statusTitles = map[string]string{
	"held":            "Stand ditahan",
	"payment_pending": "Menunggu pembayaran",
	"confirmed":       "Pembayaran diterima, booking terkonfirmasi",
	"expired":         "Waktu hold habis",
	"cancelled":       "Booking dibatalkan",
}

// BookingFundsFor assembles the timeline from booking history, check-in, refunds, and disputes.
func (s Store) BookingFundsFor(ctx context.Context, actor, bookingID string) (BookingFunds, error) {
	out := BookingFunds{Refunds: []Refund{}, Timeline: []TimelineItem{}}
	if !httpapi.ValidID(actor) || !httpapi.ValidID(bookingID) {
		return out, ErrNotFound
	}
	err := pgx.BeginTxFunc(ctx, s.DB, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var eventID string
		if err := tx.QueryRow(ctx, `SELECT b.id::text, b.status, b.event_id::text FROM bookings b WHERE b.id = $1 AND `+tenantGuard,
			bookingID, actor).Scan(&out.BookingID, &out.Status, &eventID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT created_at, to_status, COALESCE(reason, '') FROM booking_status_history
			WHERE booking_id = $1 ORDER BY id`, bookingID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var item TimelineItem
			var status, reason string
			if err := rows.Scan(&item.At, &status, &reason); err != nil {
				return err
			}
			item.Kind, item.Title = status, statusTitles[status]
			if status == "cancelled" && reason == "event_cancelled" {
				item.Title = "Acara dibatalkan oleh penyelenggara"
			}
			out.Timeline = append(out.Timeline, item)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		var checked time.Time
		if err := tx.QueryRow(ctx, `SELECT created_at FROM check_ins WHERE booking_id = $1`, bookingID).Scan(&checked); err == nil {
			out.CheckedIn = &checked
			out.Timeline = append(out.Timeline, TimelineItem{At: checked, Kind: "checked_in", Title: "Check-in di lokasi"})
		} else if err != pgx.ErrNoRows {
			return err
		}
		refundRows, err := tx.Query(ctx, `SELECT `+refundColumns+` FROM `+allRefunds+` WHERE r.booking_id = $1 ORDER BY r.created_at`, bookingID)
		if err != nil {
			return err
		}
		out.Refunds, err = pgx.CollectRows(refundRows, func(row pgx.CollectableRow) (Refund, error) { return scanRefund(row) })
		if err != nil {
			return err
		}
		for _, r := range out.Refunds {
			what := "Refund pembatalan acara"
			if r.Reason == "dispute" {
				what = "Refund dari sengketa"
			}
			out.Timeline = append(out.Timeline, TimelineItem{At: r.CreatedAt, Kind: "refund_requested",
				Title: fmt.Sprintf("%s diajukan", what), Detail: rupiah(r.Amount)})
			if r.RefundedAt != nil {
				out.Timeline = append(out.Timeline, TimelineItem{At: *r.RefundedAt, Kind: "refund_refunded",
					Title: fmt.Sprintf("%s dikirim", what), Detail: rupiah(r.Amount)})
			}
		}
		disputes, err := tx.Query(ctx, `SELECT `+disputeColumns+` FROM disputes d WHERE d.booking_id = $1 ORDER BY d.created_at`, bookingID)
		if err != nil {
			return err
		}
		all, err := pgx.CollectRows(disputes, func(row pgx.CollectableRow) (Dispute, error) { return scanDispute(row) })
		if err != nil {
			return err
		}
		for i := range all {
			d := all[i]
			out.Dispute = &all[i]
			out.Timeline = append(out.Timeline, TimelineItem{At: d.CreatedAt, Kind: "dispute_opened", Title: "Sengketa dibuka"})
			if d.ResolvedAt != nil {
				title := "Sengketa ditolak"
				if d.Status == "refunded" {
					title = "Sengketa selesai dengan refund"
				}
				item := TimelineItem{At: *d.ResolvedAt, Kind: "dispute_resolved", Title: title}
				if d.ResolutionNote != nil {
					item.Detail = *d.ResolutionNote
				}
				out.Timeline = append(out.Timeline, item)
			}
		}
		var paid, released, open bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM ledger_transactions t JOIN payment_attempts a ON a.id = t.ref_id
				WHERE t.kind = 'payment_received' AND t.ref_type = 'payment_attempt' AND a.booking_id = $1),
			EXISTS (SELECT 1 FROM payouts WHERE event_id = $2 AND status = 'released'),
			EXISTS (SELECT 1 FROM disputes WHERE booking_id = $1 AND status = 'open')`, bookingID, eventID).Scan(&paid, &released, &open); err != nil {
			return err
		}
		out.CanDispute = paid && !released && !open
		sort.SliceStable(out.Timeline, func(i, j int) bool { return out.Timeline[i].At.Before(out.Timeline[j].At) })
		return nil
	})
	return out, err
}

// RefundRow and DisputeRow add who and what, for organizer and admin lists.
type RefundRow struct {
	Refund
	BusinessName string `json:"business_name"`
	EventName    string `json:"event_name"`
}

type DisputeRow struct {
	Dispute
	BusinessName string `json:"business_name"`
	EventName    string `json:"event_name"`
}

type PayoutRow struct {
	Payout
	EventName string `json:"event_name"`
}

// OrganizerFunds is what the organizer's funds page shows for one event.
type OrganizerFunds struct {
	Payout   PayoutState  `json:"payout"`
	Refunds  []RefundRow  `json:"refunds"`
	Disputes []DisputeRow `json:"disputes"`
}

func listRefunds(ctx context.Context, tx pgx.Tx, where string, args ...any) ([]RefundRow, error) {
	rows, err := tx.Query(ctx, `SELECT `+refundColumns+`, z.name, e.name FROM `+allRefunds+`
		JOIN bookings b ON b.id = r.booking_id JOIN businesses z ON z.id = b.business_id JOIN events e ON e.id = r.event_id
		WHERE `+where+` ORDER BY r.created_at DESC, r.id LIMIT 200`, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (RefundRow, error) {
		var x RefundRow
		err := row.Scan(&x.ID, &x.BookingID, &x.EventID, &x.Reason, &x.Amount, &x.RentAmount, &x.Fee, &x.Mode, &x.Status,
			&x.ProviderRef, &x.Failure, &x.CreatedAt, &x.RefundedAt, &x.BusinessName, &x.EventName)
		return x, err
	})
}

func listDisputes(ctx context.Context, tx pgx.Tx, where string, args ...any) ([]DisputeRow, error) {
	rows, err := tx.Query(ctx, `SELECT `+disputeColumns+`, z.name, e.name FROM disputes d
		JOIN bookings b ON b.id = d.booking_id JOIN businesses z ON z.id = b.business_id JOIN events e ON e.id = d.event_id
		WHERE `+where+` ORDER BY d.created_at DESC, d.id LIMIT 200`, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (DisputeRow, error) {
		var x DisputeRow
		err := row.Scan(&x.ID, &x.BookingID, &x.EventID, &x.Reason, &x.Status, &x.ResolutionNote, &x.RefundAmount,
			&x.CreatedAt, &x.ResolvedAt, &x.BusinessName, &x.EventName)
		return x, err
	})
}

// OrganizerFundsFor lists refunds, disputes, and payout eligibility for the organization's staff.
func (s Store) OrganizerFundsFor(ctx context.Context, actor, eventID string) (OrganizerFunds, error) {
	out := OrganizerFunds{Refunds: []RefundRow{}, Disputes: []DisputeRow{}}
	if !httpapi.ValidID(actor) || !httpapi.ValidID(eventID) {
		return out, ErrNotFound
	}
	err := pgx.BeginTxFunc(ctx, s.DB, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		staff, err := isStaff(ctx, tx, actor, eventID)
		if err != nil {
			return err
		}
		if !staff {
			return ErrNotFound
		}
		if out.Payout, _, err = payoutState(ctx, tx, eventID); err != nil {
			return err
		}
		if out.Refunds, err = listRefunds(ctx, tx, `r.event_id = $1`, eventID); err != nil {
			return err
		}
		out.Disputes, err = listDisputes(ctx, tx, `d.event_id = $1`, eventID)
		return err
	})
	return out, err
}

// AdminQueue is everything an operator has to act on.
type AdminQueue struct {
	Refunds  []RefundRow  `json:"refunds"`
	Payouts  []PayoutRow  `json:"payouts"`
	Disputes []DisputeRow `json:"disputes"`
}

// AdminQueueFor lists refunds that are not paid out, payouts waiting for confirmation, and open disputes.
func (s Store) AdminQueueFor(ctx context.Context, admin string) (AdminQueue, error) {
	out := AdminQueue{Refunds: []RefundRow{}, Payouts: []PayoutRow{}, Disputes: []DisputeRow{}}
	err := pgx.BeginTxFunc(ctx, s.DB, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if err := requireAdmin(ctx, tx, admin); err != nil {
			return err
		}
		var err error
		if out.Refunds, err = listRefunds(ctx, tx, `r.status <> 'refunded'`); err != nil {
			return err
		}
		if out.Disputes, err = listDisputes(ctx, tx, `d.status = 'open'`); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+payoutColumns+`, e.name FROM payouts p JOIN events e ON e.id = p.event_id
			WHERE p.status = 'requested' ORDER BY p.requested_at LIMIT 200`)
		if err != nil {
			return err
		}
		out.Payouts, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (PayoutRow, error) {
			var x PayoutRow
			err := row.Scan(&x.ID, &x.EventID, &x.Amount, &x.Status, &x.Bank, &x.Account, &x.Holder, &x.ProviderRef,
				&x.Failure, &x.RequestedAt, &x.ReleasedAt, &x.EventName)
			return x, err
		})
		return err
	})
	return out, err
}
