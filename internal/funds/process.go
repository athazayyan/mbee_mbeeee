package funds

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"kitakarsa.com/tenantku/backend/internal/payment"
)

// maxProviderTries is how often a refund is retried before a person is asked to step in.
const maxProviderTries = 5

// ProcessReport counts what one pass did.
type ProcessReport struct {
	Refunded       int `json:"refunded"`
	ManualRequired int `json:"manual_required"`
	Retrying       int `json:"retrying"`
}

// Process sends refunds owed after dispute decisions. Refunds for a cancelled event are handled by the
// worker in the payment package. Every step is idempotent, so the command can run on a timer and after a crash.
func (s Store) Process(ctx context.Context) (ProcessReport, error) {
	var report ProcessReport
	if s.DB == nil {
		return report, ErrUnavailable
	}
	err := s.sendRefunds(ctx, &report)
	return report, err
}

func (s Store) sendRefunds(ctx context.Context, report *ProcessReport) error {
	rows, err := s.DB.Query(ctx, `SELECT id::text FROM refunds WHERE status = 'requested' ORDER BY created_at LIMIT 100`)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error { return s.sendRefund(ctx, tx, id, report) }); err != nil {
			return err
		}
	}
	return nil
}

func (s Store) sendRefund(ctx context.Context, tx pgx.Tx, id string, report *ProcessReport) error {
	r, err := scanRefund(tx.QueryRow(ctx, `SELECT `+refundColumns+` FROM refunds r WHERE r.id = $1 AND r.status = 'requested'
		FOR UPDATE OF r SKIP LOCKED`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // another pass took it
	}
	if err != nil {
		return err
	}
	var attemptID string
	var tries int
	if err := tx.QueryRow(ctx, `SELECT attempt_id::text, attempts FROM refunds WHERE id = $1`, id).Scan(&attemptID, &tries); err != nil {
		return err
	}
	if r.Mode == "api" && s.Provider != nil {
		result, err := s.Provider.Refund(ctx, attemptID, r.ID, r.Amount, "Sengketa diselesaikan admin")
		switch {
		case err == nil:
			if _, err := confirmRefund(ctx, tx, r.ID, result.Reference, 0); err != nil {
				return err
			}
			report.Refunded++
			return nil
		case errors.Is(err, payment.ErrRefundUnsupported):
			// A retry after a lost response is refused as a duplicate. The transaction status tells the difference.
			if status, serr := s.Provider.Status(ctx, attemptID); serr == nil &&
				(status.TransactionStatus == "refund" || status.TransactionStatus == "partial_refund") {
				if _, err := confirmRefund(ctx, tx, r.ID, "", 0); err != nil {
					return err
				}
				report.Refunded++
				return nil
			}
			return s.needsPerson(ctx, tx, report, r, "Metode pembayaran ini tidak mendukung refund otomatis")
		default:
			if tries+1 < maxProviderTries {
				_, err := tx.Exec(ctx, `UPDATE refunds SET attempts = attempts + 1, updated_at = clock_timestamp(),
					failure = 'provider unavailable' WHERE id = $1`, id)
				report.Retrying++
				return err
			}
			return s.needsPerson(ctx, tx, report, r, "Penyedia pembayaran gagal memproses refund berulang kali")
		}
	}
	return s.needsPerson(ctx, tx, report, r, "Dikembalikan manual ke rekening tenant")
}

// needsPerson parks a refund for an operator and puts it on the exception queue, once.
func (s Store) needsPerson(ctx context.Context, tx pgx.Tx, report *ProcessReport, r Refund, why string) error {
	if _, err := tx.Exec(ctx, `UPDATE refunds SET status = 'manual_required', mode = 'manual', failure = $2,
		updated_at = clock_timestamp() WHERE id = $1`, r.ID, why); err != nil {
		return err
	}
	detail, _ := json.Marshal(map[string]any{"refund_id": r.ID, "amount": r.Amount, "reason": why})
	_, err := tx.Exec(ctx, `INSERT INTO payment_exceptions (attempt_id, booking_id, kind, detail)
		SELECT a.id, a.booking_id, 'manual_refund_required', $2 FROM refunds f JOIN payment_attempts a ON a.id = f.attempt_id
		WHERE f.id = $1 AND NOT EXISTS (SELECT 1 FROM payment_exceptions x WHERE x.kind = 'manual_refund_required'
			AND x.status = 'open' AND x.detail->>'refund_id' = $1::text)`, r.ID, detail)
	if err == nil {
		report.ManualRequired++
	}
	return err
}
