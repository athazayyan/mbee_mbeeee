package inventory

import (
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

const (
	saleSchemaVersion = 1
	maxSaleItems      = 100
	maxSaleQuantity   = 10_000
	maxSalesPerBatch  = 100
	// Jam perangkat kasir offline bisa melenceng; transaksi lebih dari sehari di masa depan ditolak.
	maxClockAhead = 24 * time.Hour
)

var earliestSale = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

type PosSession struct {
	ID         string    `json:"id"`
	BusinessID string    `json:"business_id"`
	DeviceID   string    `json:"device_id"`
	EventID    *string   `json:"event_id,omitempty"`
	OpenedAt   time.Time `json:"opened_at"`
	Status     string    `json:"status"`
	closedAt   *time.Time
}

type posSessionInput struct {
	DeviceID string  `json:"device_id"`
	EventID  *string `json:"event_id"`
}

type saleItemInput struct {
	ProductID   string `json:"product_id"`
	ProductName string `json:"product_name"`
	Quantity    int64  `json:"quantity"`
	UnitPrice   int64  `json:"unit_price"`
	UnitCost    *int64 `json:"unit_cost,omitempty"`
	Discount    int64  `json:"discount,omitempty"`
	Subtotal    int64  `json:"subtotal"`
}

type saleInput struct {
	SaleID          string          `json:"sale_id"`
	DeviceID        string          `json:"device_id"`
	BusinessID      string          `json:"business_id"`
	SessionID       string          `json:"session_id"`
	EventID         *string         `json:"event_id,omitempty"`
	ClientCreatedAt time.Time       `json:"client_created_at"`
	SchemaVersion   int             `json:"schema_version"`
	PayloadHash     string          `json:"payload_hash"`
	Items           []saleItemInput `json:"items"`
	Currency        string          `json:"currency"`
	Method          string          `json:"method"`
	Discount        int64           `json:"discount,omitempty"`
	Total           int64           `json:"total"`
}

type salesBatch struct {
	Sales []saleInput `json:"sales"`
}

type SaleItem struct {
	ID          string `json:"id"`
	ProductID   string `json:"product_id"`
	ProductName string `json:"product_name"`
	Quantity    int64  `json:"quantity"`
	UnitPrice   int64  `json:"unit_price"`
	UnitCost    *int64 `json:"unit_cost,omitempty"`
	Discount    int64  `json:"discount"`
	Subtotal    int64  `json:"subtotal"`
	// ReturnedQuantity adalah jumlah yang sudah dikembalikan lewat void atau retur.
	ReturnedQuantity int64 `json:"returned_quantity"`
}

type Sale struct {
	ID              string     `json:"id"`
	ReceivedAt      time.Time  `json:"received_at"`
	SaleID          string     `json:"sale_id"`
	DeviceID        string     `json:"device_id"`
	BusinessID      string     `json:"business_id"`
	SessionID       string     `json:"session_id"`
	EventID         *string    `json:"event_id,omitempty"`
	ClientCreatedAt time.Time  `json:"client_created_at"`
	SchemaVersion   int        `json:"schema_version"`
	PayloadHash     string     `json:"payload_hash"`
	Items           []SaleItem `json:"items"`
	Currency        string     `json:"currency"`
	Method          string     `json:"method"`
	Discount        int64      `json:"discount"`
	Total           int64      `json:"total"`
	// RefundedAmount adalah total nilai void dan retur; omzet bersih = Total - RefundedAmount.
	RefundedAmount int64 `json:"refunded_amount"`
	Voided         bool  `json:"voided"`
}

// syncOutcome adalah hasil per transaksi; Code terisi untuk rejected dan retryable.
type syncOutcome struct {
	SaleID   string `json:"sale_id"`
	Status   string `json:"status"`
	ServerID string `json:"server_id,omitempty"`
	Code     string `json:"code,omitempty"`
}

type rejection string

func (r rejection) Error() string { return "sale rejected: " + string(r) }

func multiplyRupiah(price, quantity int64) (int64, bool) {
	if quantity != 0 && price > maxRupiah/quantity {
		return 0, false
	}
	return price * quantity, true
}

// validateSale memeriksa satu transaksi dan menormalkan isinya. Total dihitung ulang di server.
func validateSale(sale *saleInput, now time.Time) rejection {
	for _, id := range []string{sale.SaleID, sale.DeviceID, sale.BusinessID, sale.SessionID} {
		if !httpapi.ValidID(id) {
			return "INVALID_SALE"
		}
	}
	sale.SaleID, sale.DeviceID = strings.ToLower(sale.SaleID), strings.ToLower(sale.DeviceID)
	sale.BusinessID, sale.SessionID = strings.ToLower(sale.BusinessID), strings.ToLower(sale.SessionID)
	if sale.EventID != nil {
		if !httpapi.ValidID(*sale.EventID) {
			return "INVALID_SALE"
		}
		event := strings.ToLower(*sale.EventID)
		sale.EventID = &event
	}
	if sale.SchemaVersion != saleSchemaVersion {
		return "UNSUPPORTED_SCHEMA_VERSION"
	}
	if !httpapi.ValidSingleLine(sale.PayloadHash, 128) || sale.Currency != "IDR" ||
		(sale.Method != "cash" && sale.Method != "external") {
		return "INVALID_SALE"
	}
	if sale.ClientCreatedAt.IsZero() || sale.ClientCreatedAt.Before(earliestSale) ||
		sale.ClientCreatedAt.After(now.Add(maxClockAhead)) {
		return "INVALID_TIMESTAMP"
	}
	sale.ClientCreatedAt = sale.ClientCreatedAt.UTC()
	if len(sale.Items) == 0 || len(sale.Items) > maxSaleItems {
		return "INVALID_ITEMS"
	}
	var sum int64
	for index := range sale.Items {
		item := &sale.Items[index]
		item.ProductName = strings.TrimSpace(item.ProductName)
		if !httpapi.ValidID(item.ProductID) || !httpapi.ValidSingleLine(item.ProductName, 120) ||
			item.Quantity < 1 || item.Quantity > maxSaleQuantity || item.UnitPrice < 0 || item.UnitPrice > maxRupiah ||
			!validRupiah(item.UnitCost) || item.Discount < 0 {
			return "INVALID_ITEMS"
		}
		item.ProductID = strings.ToLower(item.ProductID)
		gross, ok := multiplyRupiah(item.UnitPrice, item.Quantity)
		if !ok || item.Discount > gross || item.Subtotal != gross-item.Discount {
			return "INVALID_TOTAL"
		}
		sum += item.Subtotal
		if sum > maxRupiah {
			return "INVALID_TOTAL"
		}
	}
	if sale.Discount < 0 || sale.Discount > sum || sale.Total != sum-sale.Discount {
		return "INVALID_TOTAL"
	}
	return ""
}

// saleDigest membuat sidik kanonik transaksi yang sudah dinormalkan untuk dedupe sync ulang.
func saleDigest(sale saleInput) []byte {
	encoded, _ := json.Marshal(sale)
	digest := sha256.Sum256(encoded)
	return digest[:]
}

func validateSessionInput(input *posSessionInput) fieldErrors {
	fields := make(fieldErrors)
	if !httpapi.ValidID(input.DeviceID) {
		fields["device_id"] = "Perangkat kasir tidak dikenali. Muat ulang halaman kasir."
	} else {
		input.DeviceID = strings.ToLower(input.DeviceID)
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

func sessionStatus(session *PosSession) {
	session.Status = "open"
	if session.closedAt != nil {
		session.Status = "closed"
	}
}

func scanSession(row pgx.Row) (PosSession, error) {
	var session PosSession
	err := row.Scan(&session.ID, &session.BusinessID, &session.DeviceID, &session.EventID, &session.OpenedAt, &session.closedAt)
	sessionStatus(&session)
	return session, err
}

const sessionColumns = `id::text, business_id::text, device_id::text, event_id::text, opened_at, closed_at`

// openSession mengembalikan sesi terbuka perangkat ini, atau membuka sesi baru. Sesi lama untuk acara
// berbeda ditutup lebih dulu.
func (s Store) openSession(ctx context.Context, actor, business string, input posSessionInput) (PosSession, bool, error) {
	var result PosSession
	created := false
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if err := requireMember(ctx, tx, actor, business, false); err != nil {
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
		current, err := scanSession(tx.QueryRow(ctx, `SELECT `+sessionColumns+` FROM pos_sessions
			WHERE business_id = $1 AND device_id = $2 AND closed_at IS NULL FOR UPDATE`, business, input.DeviceID))
		switch {
		case err == nil && equalOptional(current.EventID, input.EventID):
			result = current
			return nil
		case err == nil:
			if _, err := tx.Exec(ctx, `UPDATE pos_sessions SET closed_at = clock_timestamp() WHERE id = $1`, current.ID); err != nil {
				return err
			}
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		result, err = scanSession(tx.QueryRow(ctx, `INSERT INTO pos_sessions (business_id, device_id, event_id, opened_by)
			VALUES ($1, $2, $3, $4) RETURNING `+sessionColumns, business, input.DeviceID, input.EventID, actor))
		created = err == nil
		return err
	})
	return result, created, err
}

func equalOptional(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// syncSale menyimpan satu transaksi dalam transaksinya sendiri agar kegagalan satu item tidak membatalkan item lain.
func (s Store) syncSale(ctx context.Context, actor string, sale saleInput) syncOutcome {
	outcome := syncOutcome{SaleID: sale.SaleID}
	digest := saleDigest(sale)
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if err := requireMember(ctx, tx, actor, sale.BusinessID, false); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return rejection("BUSINESS_FORBIDDEN")
			}
			return err
		}
		if duplicate, err := findDuplicate(ctx, tx, sale.BusinessID, sale.SaleID, digest); err != nil || duplicate != "" {
			outcome.ServerID = duplicate
			return err
		}
		var sessionFound bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pos_sessions
			WHERE business_id = $1 AND id = $2 AND device_id = $3)`, sale.BusinessID, sale.SessionID, sale.DeviceID).
			Scan(&sessionFound); err != nil {
			return err
		}
		if !sessionFound {
			return rejection("UNKNOWN_SESSION")
		}
		if sale.EventID != nil {
			var eventFound bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM events WHERE id = $1)`, *sale.EventID).Scan(&eventFound); err != nil {
				return err
			}
			if !eventFound {
				return rejection("UNKNOWN_EVENT")
			}
		}
		quantities := make(map[string]int64)
		for _, item := range sale.Items {
			quantities[item.ProductID] += item.Quantity
		}
		products := make([]string, 0, len(quantities))
		for id := range quantities {
			products = append(products, id)
		}
		// Urutan kunci tetap mencegah deadlock antar-sync yang menjual produk yang sama.
		slices.Sort(products)
		var known int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM products WHERE business_id = $1 AND id = ANY($2::uuid[])`,
			sale.BusinessID, products).Scan(&known); err != nil {
			return err
		}
		if known != len(products) {
			return rejection("UNKNOWN_PRODUCT")
		}
		var serverID string
		err := tx.QueryRow(ctx, `INSERT INTO sales (business_id, sale_id, device_id, session_id, event_id, client_created_at,
			schema_version, payload_hash, request_digest, currency, method, discount, total, synced_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
			ON CONFLICT (business_id, sale_id) DO NOTHING RETURNING id::text`,
			sale.BusinessID, sale.SaleID, sale.DeviceID, sale.SessionID, sale.EventID, sale.ClientCreatedAt,
			sale.SchemaVersion, sale.PayloadHash, digest, sale.Currency, sale.Method, sale.Discount, sale.Total, actor).
			Scan(&serverID)
		if errors.Is(err, pgx.ErrNoRows) {
			// Sync paralel dengan sale_id sama baru saja tersimpan.
			duplicate, err := findDuplicate(ctx, tx, sale.BusinessID, sale.SaleID, digest)
			outcome.ServerID = duplicate
			return err
		}
		if err != nil {
			return err
		}
		outcome.ServerID = serverID
		for position, item := range sale.Items {
			if _, err := tx.Exec(ctx, `INSERT INTO sale_items (business_id, sale_ref, position, product_id, product_name,
				quantity, unit_price, unit_cost, discount, subtotal) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
				sale.BusinessID, serverID, position, item.ProductID, item.ProductName, item.Quantity, item.UnitPrice,
				item.UnitCost, item.Discount, item.Subtotal); err != nil {
				return err
			}
		}
		for _, product := range products {
			if _, err := tx.Exec(ctx, `UPDATE products SET stock = stock - $2, updated_at = clock_timestamp() WHERE id = $1`,
				product, quantities[product]); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO inventory_movements (business_id, product_id, delta, reason, actor_id, sale_ref)
				VALUES ($1, $2, $3, 'sale', $4, $5)`, sale.BusinessID, product, -quantities[product], actor, serverID); err != nil {
				return err
			}
		}
		outcome.Status = "accepted"
		return nil
	})
	var rejected rejection
	switch {
	case errors.As(err, &rejected):
		return syncOutcome{SaleID: sale.SaleID, Status: "rejected", Code: string(rejected)}
	case err != nil:
		return syncOutcome{SaleID: sale.SaleID, Status: "retryable", Code: "INTERNAL_ERROR"}
	case outcome.Status == "":
		outcome.Status = "duplicate"
	}
	return outcome
}

var errPayloadConflict = rejection("SYNC_PAYLOAD_CONFLICT")

// findDuplicate mengembalikan id server bila sale_id sudah tersimpan dengan isi yang sama.
func findDuplicate(ctx context.Context, tx pgx.Tx, business, saleID string, digest []byte) (string, error) {
	var serverID string
	var stored []byte
	err := tx.QueryRow(ctx, `SELECT id::text, request_digest FROM sales WHERE business_id = $1 AND sale_id = $2`,
		business, saleID).Scan(&serverID, &stored)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", nil
	case err != nil:
		return "", err
	case string(stored) != string(digest):
		return "", errPayloadConflict
	}
	return serverID, nil
}

type salesFilter struct {
	From, To time.Time
	EventID  *string
	AfterAt  time.Time
	AfterID  string
	Limit    int
}

func (s Store) listSales(ctx context.Context, actor, business string, filter salesFilter) ([]Sale, error) {
	var result []Sale
	err := pgx.BeginTxFunc(ctx, s.DB, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if err := requireMember(ctx, tx, actor, business, false); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT id::text, received_at, sale_id::text, device_id::text, business_id::text,
			session_id::text, event_id::text, client_created_at, schema_version, payload_hash, currency, method, discount, total,
			(SELECT COALESCE(sum(a.amount), 0) FROM sale_adjustments a WHERE a.sale_ref = sales.id),
			EXISTS (SELECT 1 FROM sale_adjustments a WHERE a.sale_ref = sales.id AND a.type = 'void')
			FROM sales WHERE business_id = $1
			AND ($2::timestamptz IS NULL OR client_created_at >= $2)
			AND ($3::timestamptz IS NULL OR client_created_at < $3)
			AND ($4::uuid IS NULL OR event_id = $4)
			AND ($5::uuid IS NULL OR (client_created_at, id) < ($6, $5::uuid))
			ORDER BY client_created_at DESC, id DESC LIMIT $7`,
			business, nilIfZero(filter.From), nilIfZero(filter.To), filter.EventID, nilIfEmpty(filter.AfterID),
			filter.AfterAt, filter.Limit)
		if err != nil {
			return err
		}
		result, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Sale, error) {
			var sale Sale
			err := row.Scan(&sale.ID, &sale.ReceivedAt, &sale.SaleID, &sale.DeviceID, &sale.BusinessID, &sale.SessionID,
				&sale.EventID, &sale.ClientCreatedAt, &sale.SchemaVersion, &sale.PayloadHash, &sale.Currency, &sale.Method,
				&sale.Discount, &sale.Total, &sale.RefundedAmount, &sale.Voided)
			sale.Items = []SaleItem{}
			return sale, err
		})
		if err != nil || len(result) == 0 {
			return err
		}
		index := make(map[string]int, len(result))
		ids := make([]string, len(result))
		for i, sale := range result {
			index[sale.ID] = i
			ids[i] = sale.ID
		}
		items, err := tx.Query(ctx, `SELECT sale_ref::text, id::text, product_id::text, product_name, quantity, unit_price,
			unit_cost, discount, subtotal,
			(SELECT COALESCE(sum(ai.quantity), 0) FROM sale_adjustment_items ai WHERE ai.sale_item_id = sale_items.id)
			FROM sale_items WHERE sale_ref = ANY($1::uuid[]) ORDER BY sale_ref, position`, ids)
		if err != nil {
			return err
		}
		defer items.Close()
		for items.Next() {
			var ref string
			var item SaleItem
			if err := items.Scan(&ref, &item.ID, &item.ProductID, &item.ProductName, &item.Quantity, &item.UnitPrice,
				&item.UnitCost, &item.Discount, &item.Subtotal, &item.ReturnedQuantity); err != nil {
				return err
			}
			result[index[ref]].Items = append(result[index[ref]].Items, item)
		}
		return items.Err()
	})
	return result, err
}

func nilIfZero(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	return &value
}
