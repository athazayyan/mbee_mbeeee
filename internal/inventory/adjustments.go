package inventory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

var (
	errIdempotencyMismatch = errors.New("idempotency key reused with a different payload")
	errSaleVoided          = errors.New("sale already voided")
	errNothingToAdjust     = errors.New("every item has been returned")
)

type adjustmentItemInput struct {
	SaleItemID string `json:"sale_item_id"`
	Quantity   int64  `json:"quantity"`
}

type adjustmentInput struct {
	Type   string                `json:"type"`
	Reason string                `json:"reason"`
	Items  []adjustmentItemInput `json:"items,omitempty"`
}

type AdjustmentResult struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Amount int64  `json:"amount"`
}

// validateAdjustment: void membatalkan seluruh sisa transaksi tanpa daftar barang; retur memilih barang dan jumlah.
func validateAdjustment(input *adjustmentInput) fieldErrors {
	fields := make(fieldErrors)
	input.Reason = strings.TrimSpace(input.Reason)
	if !httpapi.ValidSingleLine(input.Reason, 300) {
		fields["reason"] = "Tulis alasan satu baris hingga 300 karakter."
	}
	switch input.Type {
	case "void":
		if len(input.Items) > 0 {
			fields["items"] = "Void membatalkan seluruh sisa transaksi; jangan pilih barang."
		}
	case "return":
		if len(input.Items) == 0 || len(input.Items) > maxSaleItems {
			fields["items"] = "Pilih barang yang diretur."
			break
		}
		seen := make(map[string]bool, len(input.Items))
		for index := range input.Items {
			item := &input.Items[index]
			item.SaleItemID = strings.ToLower(item.SaleItemID)
			if !httpapi.ValidID(item.SaleItemID) || seen[item.SaleItemID] || item.Quantity < 1 || item.Quantity > maxSaleQuantity {
				fields["items"] = "Periksa barang dan jumlah retur."
				break
			}
			seen[item.SaleItemID] = true
		}
		// Urutan tetap agar retry dengan isi sama menghasilkan sidik yang sama.
		slices.SortFunc(input.Items, func(a, b adjustmentItemInput) int { return strings.Compare(a.SaleItemID, b.SaleItemID) })
	default:
		fields["type"] = "Pilih void atau return."
	}
	if len(fields) == 0 {
		return nil
	}
	return fields
}

type refundLine struct {
	quantity, subtotal, remaining, take int64
}

// computeRefund menghitung nilai pengembalian. Bagian tiap barang = subtotal × jumlah / kuantitas, lalu
// potongan transaksi dibagi proporsional. Bila semua sisa barang dikembalikan, nilainya tepat sisa total,
// sehingga jumlah seluruh pengembalian tidak pernah melebihi total transaksi.
func computeRefund(lines []refundLine, total, refunded int64) int64 {
	var gross, part int64
	finishing := true
	for _, line := range lines {
		gross += line.subtotal
		if line.take > 0 {
			part += line.subtotal * line.take / line.quantity
		}
		if line.take < line.remaining {
			finishing = false
		}
	}
	left := max(total-refunded, 0)
	if finishing {
		return left
	}
	if gross == 0 {
		return 0
	}
	return min(part*total/gross, left)
}

func adjustmentDigest(saleID string, input adjustmentInput) []byte {
	encoded, _ := json.Marshal(struct {
		Sale string `json:"sale"`
		adjustmentInput
	}{saleID, input})
	digest := sha256.Sum256(encoded)
	return digest[:]
}

type saleLine struct {
	id, product         string
	quantity, subtotal  int64
	returned, remaining int64
}

// adjustSale mencatat void atau retur sebagai entri baru dan mengembalikan stok sekali.
func (s Store) adjustSale(ctx context.Context, actor, saleID, key string, input adjustmentInput) (AdjustmentResult, bool, error) {
	var result AdjustmentResult
	created := false
	digest := adjustmentDigest(saleID, input)
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var business string
		var total int64
		if err := tx.QueryRow(ctx, `SELECT s.business_id::text, s.total FROM sales s
			JOIN memberships m ON m.business_id = s.business_id AND m.user_id = $1 AND m.role = 'owner'
			WHERE s.id = $2`, actor, saleID).Scan(&business, &total); err != nil {
			return err
		}
		// Koreksi bersamaan pada transaksi yang sama diproses satu per satu.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "sale:"+saleID); err != nil {
			return err
		}
		var stored []byte
		err := tx.QueryRow(ctx, `SELECT id::text, type, amount, request_digest FROM sale_adjustments
			WHERE business_id = $1 AND created_by = $2 AND idempotency_key = $3`, business, actor, key).
			Scan(&result.ID, &result.Status, &result.Amount, &stored)
		switch {
		case err == nil && !bytes.Equal(stored, digest):
			return errIdempotencyMismatch
		case err == nil:
			result.Status = adjustmentStatus(result.Status)
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		var voided bool
		var refunded int64
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM sale_adjustments WHERE sale_ref = $1 AND type = 'void'),
			COALESCE((SELECT sum(amount) FROM sale_adjustments WHERE sale_ref = $1), 0)`, saleID).Scan(&voided, &refunded); err != nil {
			return err
		}
		if voided {
			return errSaleVoided
		}
		rows, err := tx.Query(ctx, `SELECT si.id::text, si.product_id::text, si.quantity, si.subtotal,
			COALESCE((SELECT sum(ai.quantity) FROM sale_adjustment_items ai WHERE ai.sale_item_id = si.id), 0)
			FROM sale_items si WHERE si.sale_ref = $1 ORDER BY si.position`, saleID)
		if err != nil {
			return err
		}
		lines, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (saleLine, error) {
			var line saleLine
			err := row.Scan(&line.id, &line.product, &line.quantity, &line.subtotal, &line.returned)
			line.remaining = line.quantity - line.returned
			return line, err
		})
		if err != nil {
			return err
		}
		take := make(map[string]int64)
		if input.Type == "void" {
			for _, line := range lines {
				if line.remaining > 0 {
					take[line.id] = line.remaining
				}
			}
		} else {
			for _, item := range input.Items {
				index := slices.IndexFunc(lines, func(line saleLine) bool { return line.id == item.SaleItemID })
				if index < 0 {
					return fieldErrors{"items": "Barang tidak ada di transaksi ini."}
				}
				if item.Quantity > lines[index].remaining {
					return fieldErrors{"items": "Jumlah retur melebihi sisa barang pada transaksi."}
				}
				take[item.SaleItemID] = item.Quantity
			}
		}
		if len(take) == 0 {
			return errNothingToAdjust
		}
		refundLines := make([]refundLine, len(lines))
		for index, line := range lines {
			refundLines[index] = refundLine{quantity: line.quantity, subtotal: line.subtotal, remaining: line.remaining, take: take[line.id]}
		}
		result.Amount = computeRefund(refundLines, total, refunded)
		if err := tx.QueryRow(ctx, `INSERT INTO sale_adjustments
			(business_id, sale_ref, type, reason, amount, created_by, idempotency_key, request_digest)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id::text`,
			business, saleID, input.Type, input.Reason, result.Amount, actor, key, digest).Scan(&result.ID); err != nil {
			return err
		}
		restock := make(map[string]int64)
		for _, line := range lines {
			if take[line.id] == 0 {
				continue
			}
			if _, err := tx.Exec(ctx, `INSERT INTO sale_adjustment_items (adjustment_id, sale_item_id, quantity)
				VALUES ($1, $2, $3)`, result.ID, line.id, take[line.id]); err != nil {
				return err
			}
			restock[line.product] += take[line.id]
		}
		products := make([]string, 0, len(restock))
		for product := range restock {
			products = append(products, product)
		}
		slices.Sort(products)
		for _, product := range products {
			if _, err := tx.Exec(ctx, `UPDATE products SET stock = stock + $2, updated_at = clock_timestamp() WHERE id = $1`,
				product, restock[product]); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO inventory_movements (business_id, product_id, delta, reason, actor_id, adjustment_ref)
				VALUES ($1, $2, $3, $4, $5, $6)`, business, product, restock[product], input.Type, actor, result.ID); err != nil {
				return err
			}
		}
		result.Status = adjustmentStatus(input.Type)
		created = true
		return nil
	})
	return result, created, err
}

func adjustmentStatus(kind string) string {
	if kind == "void" {
		return "voided"
	}
	return "returned"
}

var expenseCategories = map[string]bool{
	"sewa": true, "bahan": true, "operasional": true, "transportasi": true, "tenaga": true, "lainnya": true,
}

type expenseInput struct {
	Amount     int64     `json:"amount"`
	Category   string    `json:"category"`
	OccurredAt time.Time `json:"occurred_at"`
	Note       *string   `json:"note,omitempty"`
	EventID    *string   `json:"event_id,omitempty"`
}

type Expense struct {
	ID         string    `json:"id"`
	BusinessID string    `json:"business_id"`
	EventID    *string   `json:"event_id,omitempty"`
	Category   string    `json:"category"`
	Amount     int64     `json:"amount"`
	OccurredAt time.Time `json:"occurred_at"`
	Note       *string   `json:"note,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

func validateExpense(input *expenseInput, now time.Time) fieldErrors {
	fields := make(fieldErrors)
	if input.Amount < 1 || input.Amount > maxRupiah {
		fields["amount"] = "Isi nominal pengeluaran dalam rupiah bulat."
	}
	if !expenseCategories[input.Category] {
		fields["category"] = "Pilih kategori pengeluaran."
	}
	if input.OccurredAt.IsZero() || input.OccurredAt.Before(earliestSale) || input.OccurredAt.After(now.Add(maxClockAhead)) {
		fields["occurred_at"] = "Isi tanggal pengeluaran yang wajar."
	}
	input.OccurredAt = input.OccurredAt.UTC()
	if input.Note != nil {
		note := strings.TrimSpace(*input.Note)
		switch {
		case note == "":
			input.Note = nil
		case !httpapi.ValidSingleLine(note, 200):
			fields["note"] = "Catatan satu baris hingga 200 karakter."
		default:
			input.Note = &note
		}
	}
	if input.EventID != nil {
		if !httpapi.ValidID(*input.EventID) {
			fields["event_id"] = "Pilih acara yang valid."
		} else {
			event := strings.ToLower(*input.EventID)
			input.EventID = &event
		}
	}
	if len(fields) == 0 {
		return nil
	}
	return fields
}

const expenseColumns = `id::text, business_id::text, event_id::text, category, amount, occurred_at, note, created_at`

func scanExpense(row pgx.Row) (Expense, error) {
	var e Expense
	err := row.Scan(&e.ID, &e.BusinessID, &e.EventID, &e.Category, &e.Amount, &e.OccurredAt, &e.Note, &e.CreatedAt)
	return e, err
}

func (s Store) createExpense(ctx context.Context, actor, business, key string, input expenseInput) (Expense, bool, error) {
	var result Expense
	created := false
	encoded, _ := json.Marshal(input)
	sum := sha256.Sum256(encoded)
	digest := sum[:]
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if err := requireMember(ctx, tx, actor, business, true); err != nil {
			return err
		}
		var stored []byte
		var err error
		result, err = scanExpenseWithDigest(tx.QueryRow(ctx, `SELECT `+expenseColumns+`, request_digest FROM expenses
			WHERE business_id = $1 AND created_by = $2 AND idempotency_key = $3`, business, actor, key), &stored)
		switch {
		case err == nil && !bytes.Equal(stored, digest):
			return errIdempotencyMismatch
		case err == nil:
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		if input.EventID != nil {
			var found bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM events WHERE id = $1)`, *input.EventID).Scan(&found); err != nil {
				return err
			}
			if !found {
				return fieldErrors{"event_id": "Acara tidak ditemukan."}
			}
		}
		result, err = scanExpense(tx.QueryRow(ctx, `INSERT INTO expenses
			(business_id, event_id, category, amount, occurred_at, note, created_by, idempotency_key, request_digest)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING `+expenseColumns,
			business, input.EventID, input.Category, input.Amount, input.OccurredAt, input.Note, actor, key, digest))
		created = err == nil
		return err
	})
	return result, created, err
}

func scanExpenseWithDigest(row pgx.Row, digest *[]byte) (Expense, error) {
	var e Expense
	err := row.Scan(&e.ID, &e.BusinessID, &e.EventID, &e.Category, &e.Amount, &e.OccurredAt, &e.Note, &e.CreatedAt, digest)
	return e, err
}

type expenseFilter struct {
	From, To time.Time
	AfterAt  time.Time
	AfterID  string
	Limit    int
}

// listExpenses mengembalikan satu halaman pengeluaran dan total nominal seluruh rentang filter.
func (s Store) listExpenses(ctx context.Context, actor, business string, filter expenseFilter) ([]Expense, int64, error) {
	var result []Expense
	var total int64
	err := pgx.BeginTxFunc(ctx, s.DB, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if err := requireMember(ctx, tx, actor, business, false); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(amount), 0) FROM expenses WHERE business_id = $1
			AND ($2::timestamptz IS NULL OR occurred_at >= $2) AND ($3::timestamptz IS NULL OR occurred_at < $3)`,
			business, nilIfZero(filter.From), nilIfZero(filter.To)).Scan(&total); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+expenseColumns+` FROM expenses WHERE business_id = $1
			AND ($2::timestamptz IS NULL OR occurred_at >= $2) AND ($3::timestamptz IS NULL OR occurred_at < $3)
			AND ($4::uuid IS NULL OR (occurred_at, id) < ($5, $4::uuid))
			ORDER BY occurred_at DESC, id DESC LIMIT $6`,
			business, nilIfZero(filter.From), nilIfZero(filter.To), nilIfEmpty(filter.AfterID), filter.AfterAt, filter.Limit)
		if err != nil {
			return err
		}
		result, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Expense, error) { return scanExpense(row) })
		return err
	})
	return result, total, err
}
