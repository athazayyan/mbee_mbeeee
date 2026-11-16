// Package inventory mengelola produk usaha dan mutasi stoknya (E10).
package inventory

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	DB *pgxpool.Pool
}

type Product struct {
	ID         string     `json:"id"`
	BusinessID string     `json:"business_id"`
	Name       string     `json:"name"`
	SKU        *string    `json:"sku,omitempty"`
	Price      int64      `json:"price"`
	Cost       *int64     `json:"cost,omitempty"`
	CategoryID *string    `json:"category_id,omitempty"`
	Stock      int32      `json:"stock"`
	ArchivedAt *time.Time `json:"archived_at,omitempty"`
	CreatedAt  time.Time  `json:"-"`
}

// optional membedakan field yang tidak dikirim dari field yang dikirim null (untuk mengosongkan nilai).
type optional[T any] struct {
	Set   bool
	Value *T
}

func (o *optional[T]) UnmarshalJSON(data []byte) error {
	o.Set = true
	if string(data) == "null" {
		o.Value = nil
		return nil
	}
	var value T
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	o.Value = &value
	return nil
}

type productInput struct {
	Name          optional[string] `json:"name"`
	SKU           optional[string] `json:"sku"`
	Price         optional[int64]  `json:"price"`
	Cost          optional[int64]  `json:"cost"`
	CategoryID    optional[string] `json:"category_id"`
	PhotoUploadID optional[string] `json:"photo_upload_id"`
	InitialStock  optional[int32]  `json:"initial_stock"`
	Archived      optional[bool]   `json:"archived"`
}

type stockInput struct {
	Delta  int32   `json:"delta"`
	Reason string  `json:"reason"`
	Note   *string `json:"note"`
}

type listFilter struct {
	Query           string
	IncludeArchived bool
	AfterAt         time.Time
	AfterID         string
	Limit           int
}

const productColumns = `p.id::text, p.business_id::text, p.name, p.sku, p.price, p.cost, p.category_id::text,
	p.stock, p.archived_at, p.created_at`

func scanProduct(row pgx.Row) (Product, error) {
	var value Product
	err := row.Scan(&value.ID, &value.BusinessID, &value.Name, &value.SKU, &value.Price, &value.Cost,
		&value.CategoryID, &value.Stock, &value.ArchivedAt, &value.CreatedAt)
	return value, err
}

func (s Store) Ready(ctx context.Context) error {
	_, err := s.DB.Exec(ctx, `SELECT 1 FROM products, inventory_movements, pos_sessions, sales, sale_items LIMIT 0`)
	return err
}

// requireMember memeriksa keanggotaan. Pemeriksaan pemilik mengunci baris keanggotaan selama penulisan;
// pembacaan berjalan di transaksi read-only sehingga tidak boleh memakai FOR SHARE.
func requireMember(ctx context.Context, tx pgx.Tx, actor, business string, ownerOnly bool) error {
	query := `SELECT business_id::text FROM memberships WHERE user_id = $1 AND business_id = $2`
	if ownerOnly {
		query += ` AND role = 'owner' FOR SHARE`
	}
	var found string
	return tx.QueryRow(ctx, query, actor, business).Scan(&found)
}

func (s Store) list(ctx context.Context, actor, business string, filter listFilter) ([]Product, error) {
	var result []Product
	err := pgx.BeginTxFunc(ctx, s.DB, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if err := requireMember(ctx, tx, actor, business, false); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+productColumns+` FROM products p
			WHERE p.business_id = $1 AND ($2 OR p.archived_at IS NULL)
			AND ($3 = '' OR strpos(lower(p.name), lower($3)) > 0 OR strpos(lower(COALESCE(p.sku, '')), lower($3)) > 0)
			AND ($4::uuid IS NULL OR (p.created_at, p.id) > ($5, $4::uuid))
			ORDER BY p.created_at, p.id LIMIT $6`,
			business, filter.IncludeArchived, filter.Query, nilIfEmpty(filter.AfterID), filter.AfterAt, filter.Limit)
		if err != nil {
			return err
		}
		result, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Product, error) { return scanProduct(row) })
		return err
	})
	return result, err
}

func (s Store) create(ctx context.Context, actor, business string, input productInput) (Product, error) {
	var result Product
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if err := requireMember(ctx, tx, actor, business, true); err != nil {
			return err
		}
		var stock int32
		if input.InitialStock.Value != nil {
			stock = *input.InitialStock.Value
		}
		var err error
		result, err = scanProduct(tx.QueryRow(ctx, `INSERT INTO products AS p
			(business_id, name, sku, price, cost, category_id, stock, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING `+productColumns,
			business, *input.Name.Value, input.SKU.Value, *input.Price.Value, input.Cost.Value,
			input.CategoryID.Value, stock, actor))
		if err != nil {
			return err
		}
		if stock != 0 {
			_, err = tx.Exec(ctx, `INSERT INTO inventory_movements (business_id, product_id, delta, reason, actor_id)
				VALUES ($1, $2, $3, 'initial', $4)`, business, result.ID, stock, actor)
		}
		return err
	})
	return result, err
}

// lockOwnedProduct mengunci produk milik usaha yang pemiliknya adalah actor.
func lockOwnedProduct(ctx context.Context, tx pgx.Tx, actor, id string) (Product, error) {
	return scanProduct(tx.QueryRow(ctx, `SELECT `+productColumns+` FROM products p
		JOIN memberships m ON m.business_id = p.business_id AND m.user_id = $1 AND m.role = 'owner'
		WHERE p.id = $2 FOR UPDATE OF p FOR SHARE OF m`, actor, id))
}

func (s Store) update(ctx context.Context, actor, id string, input productInput) (Product, error) {
	var result Product
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		current, err := lockOwnedProduct(ctx, tx, actor, id)
		if err != nil {
			return err
		}
		if input.Name.Set {
			current.Name = *input.Name.Value
		}
		if input.SKU.Set {
			current.SKU = input.SKU.Value
		}
		if input.Price.Set {
			current.Price = *input.Price.Value
		}
		if input.Cost.Set {
			current.Cost = input.Cost.Value
		}
		if input.CategoryID.Set {
			current.CategoryID = input.CategoryID.Value
		}
		archive := current.ArchivedAt != nil
		if input.Archived.Set {
			archive = *input.Archived.Value
		}
		result, err = scanProduct(tx.QueryRow(ctx, `UPDATE products AS p SET name = $2, sku = $3, price = $4,
			cost = $5, category_id = $6,
			archived_at = CASE WHEN $7 THEN COALESCE(p.archived_at, clock_timestamp()) ELSE NULL END,
			updated_at = clock_timestamp()
			WHERE p.id = $1 RETURNING `+productColumns,
			id, current.Name, current.SKU, current.Price, current.Cost, current.CategoryID, archive))
		return err
	})
	return result, err
}

var errStockOverflow = errors.New("stock would overflow")

func (s Store) adjustStock(ctx context.Context, actor, id string, input stockInput) (Product, error) {
	var result Product
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		current, err := lockOwnedProduct(ctx, tx, actor, id)
		if err != nil {
			return err
		}
		next := int64(current.Stock) + int64(input.Delta)
		if next > maxStock || next < -maxStock {
			return errStockOverflow
		}
		result, err = scanProduct(tx.QueryRow(ctx, `UPDATE products AS p SET stock = p.stock + $2,
			updated_at = clock_timestamp() WHERE p.id = $1 RETURNING `+productColumns, id, input.Delta))
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO inventory_movements (business_id, product_id, delta, reason, note, actor_id)
			VALUES ($1, $2, $3, $4, $5, $6)`, current.BusinessID, id, input.Delta, input.Reason, input.Note, actor)
		return err
	})
	return result, err
}

func nilIfEmpty(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
