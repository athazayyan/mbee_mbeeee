package admin

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"kitakarsa.com/tenantku/backend/internal/platform/adminperm"
)

// ErrNotReversible is returned for a transaction that cannot be corrected by a reversal.
var errNotReversible = errors.New("ledger transaction cannot be reversed")

// LedgerReversal is the correcting transaction posted for one ledger transaction.
type LedgerReversal struct {
	ID        string        `json:"id"`
	Reverses  string        `json:"reverses"`
	CreatedAt time.Time     `json:"created_at"`
	Entries   []LedgerEntry `json:"entries"`
}

type LedgerEntry struct {
	Account   string `json:"account"`
	Direction string `json:"direction"`
	Amount    int64  `json:"amount"`
}

// reverseLedger corrects a balance the only way an append-only ledger allows: it posts a new transaction with
// every entry flipped. A transaction is reversed at most once, and a reversal is never reversed. Asking again
// returns the existing reversal, so a retry neither doubles the correction nor writes a second audit row.
func (s Store) reverseLedger(ctx context.Context, actor, id, reason, requestID string) (LedgerReversal, bool, error) {
	var result LedgerReversal
	created := false
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if err := requireAdmin(ctx, tx, actor, adminperm.FundsManage); err != nil {
			return err
		}
		var kind string
		if err := tx.QueryRow(ctx, `SELECT kind FROM ledger_transactions WHERE id = $1`, id).Scan(&kind); err != nil {
			return err
		}
		if kind == "reversal" {
			return errNotReversible
		}
		// One reversal per transaction, even when two admins ask at the same moment.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('tenantku.admin.ledger_reversal:' || $1, 0))`, id); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `SELECT id::text, reverses::text, created_at FROM ledger_transactions WHERE kind = 'reversal' AND reverses = $1`, id).
			Scan(&result.ID, &result.Reverses, &result.CreatedAt)
		if err == nil {
			result.Entries, err = ledgerEntries(ctx, tx, result.ID)
			return err
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		original, err := ledgerEntries(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO ledger_transactions (kind, ref_type, ref_id, reverses)
			VALUES ('reversal', 'ledger_transaction', $1, $1) RETURNING id::text, reverses::text, created_at`, id).
			Scan(&result.ID, &result.Reverses, &result.CreatedAt); err != nil {
			return err
		}
		for _, entry := range original {
			flipped := "credit"
			if entry.Direction == "credit" {
				flipped = "debit"
			}
			if _, err := tx.Exec(ctx, `INSERT INTO ledger_entries (transaction_id, account, direction, amount) VALUES ($1, $2, $3, $4)`,
				result.ID, entry.Account, flipped, entry.Amount); err != nil {
				return err
			}
			result.Entries = append(result.Entries, LedgerEntry{entry.Account, flipped, entry.Amount})
		}
		created = true
		return writeAudit(ctx, tx, auditRecord{actor: actor, action: "ledger.reverse", resourceType: "ledger_transaction",
			resourceID: id, reason: reason, requestID: requestID,
			before: map[string]any{"kind": kind, "entries": original}, after: map[string]any{"reversal_id": result.ID}})
	})
	return result, created, err
}

func ledgerEntries(ctx context.Context, tx pgx.Tx, id string) ([]LedgerEntry, error) {
	rows, err := tx.Query(ctx, `SELECT account, direction, amount FROM ledger_entries WHERE transaction_id = $1 ORDER BY account, direction`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[LedgerEntry])
}
