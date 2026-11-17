package inventory

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// E10-06: perhitungan kasir, snapshot item yang tetap setelah produk berubah, sinkron paralel, dan koreksi
// void/retur yang mengembalikan stok tepat sekali. Membutuhkan TENANTKU_TEST_DATABASE_URL.
func TestCashierIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s := migratedInventoryStore(t, ctx)
	owner := scalar(t, ctx, s, `INSERT INTO users (email, password_hash) VALUES ('kasir@example.com', 'fixture') RETURNING id`)
	category := scalar(t, ctx, s, `SELECT id FROM business_categories ORDER BY id LIMIT 1`)
	business := scalar(t, ctx, s, `INSERT INTO businesses (owner_id, category_id, name) VALUES ($1, $2, 'Kopi Senja') RETURNING id`, owner, category)
	exec(t, ctx, s, `INSERT INTO memberships (user_id, business_id, role) VALUES ($1, $2, 'owner')`, owner, business)

	price, cost, stock := int64(10000), int64(6000), int32(10)
	name, otherName := "Es Kopi Susu", "Roti Bakar"
	coffee, err := s.create(ctx, owner, business, productInput{Name: optional[string]{true, &name},
		Price: optional[int64]{true, &price}, Cost: optional[int64]{true, &cost}, InitialStock: optional[int32]{true, &stock}})
	if err != nil {
		t.Fatal(err)
	}
	otherPrice := int64(8000)
	bread, err := s.create(ctx, owner, business, productInput{Name: optional[string]{true, &otherName},
		Price: optional[int64]{true, &otherPrice}, InitialStock: optional[int32]{true, &stock}})
	if err != nil {
		t.Fatal(err)
	}
	device := "6f1f9d1e-2b7c-4d5e-9a3b-1c2d3e4f5a6b"
	session, _, err := s.openSession(ctx, owner, business, posSessionInput{DeviceID: device})
	if err != nil {
		t.Fatal(err)
	}
	newSale := func(id string, items []saleItemInput, discount int64) saleInput {
		var sum int64
		for i := range items {
			items[i].Subtotal = items[i].Quantity*items[i].UnitPrice - items[i].Discount
			sum += items[i].Subtotal
		}
		return saleInput{SaleID: id, DeviceID: device, BusinessID: business, SessionID: session.ID,
			ClientCreatedAt: time.Now().Add(-time.Minute), SchemaVersion: saleSchemaVersion, PayloadHash: "hash-" + id,
			Items: items, Currency: "IDR", Method: "cash", Discount: discount, Total: sum - discount}
	}
	send := func(sale saleInput) syncOutcome {
		if code := validateSale(&sale, time.Now()); code != "" {
			return syncOutcome{SaleID: sale.SaleID, Status: "rejected", Code: string(code)}
		}
		return s.syncSale(ctx, owner, sale)
	}
	stockOf := func(id string) int64 {
		var value int64
		if err := s.DB.QueryRow(ctx, `SELECT stock FROM products WHERE id = $1`, id).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}

	// Perhitungan: total harus sama dengan jumlah item dikurangi potongan.
	first := newSale(uuid(), []saleItemInput{
		{ProductID: coffee.ID, ProductName: name, Quantity: 2, UnitPrice: price, UnitCost: &cost},
		{ProductID: bread.ID, ProductName: otherName, Quantity: 1, UnitPrice: otherPrice},
	}, 1000)
	if first.Total != 27000 {
		t.Fatalf("fixture total = %d", first.Total)
	}
	wrong := first
	wrong.SaleID, wrong.Total = uuid(), 30000
	if out := send(wrong); out.Status != "rejected" {
		t.Fatalf("wrong total accepted: %+v", out)
	}
	if out := send(first); out.Status != "accepted" {
		t.Fatalf("first sale: %+v", out)
	}
	if got := stockOf(coffee.ID); got != 8 {
		t.Fatalf("coffee stock after sale = %d, want 8", got)
	}

	// Pengiriman ulang identik tidak mengurangi stok lagi; isi berbeda dengan sale_id sama ditolak.
	if out := send(first); out.Status != "duplicate" {
		t.Fatalf("retry: %+v", out)
	}
	changed := first
	changed.PayloadHash = "hash-lain"
	changed.Discount, changed.Total = 0, 28000
	if out := send(changed); out.Status != "rejected" || out.Code != string(errPayloadConflict) {
		t.Fatalf("conflicting retry: %+v", out)
	}
	if got := stockOf(coffee.ID); got != 8 {
		t.Fatalf("coffee stock after retries = %d, want 8", got)
	}

	// Snapshot: mengubah nama dan harga produk tidak mengubah item transaksi yang sudah tersimpan.
	newName, newPrice := "Es Kopi Susu Gula Aren", int64(15000)
	if _, err := s.update(ctx, owner, coffee.ID, productInput{Name: optional[string]{true, &newName}, Price: optional[int64]{true, &newPrice}}); err != nil {
		t.Fatal(err)
	}
	var savedName string
	var savedPrice int64
	if err := s.DB.QueryRow(ctx, `SELECT i.product_name, i.unit_price FROM sale_items i JOIN sales x ON x.id = i.sale_ref
		WHERE x.sale_id = $1 AND i.product_id = $2`, first.SaleID, coffee.ID).Scan(&savedName, &savedPrice); err != nil {
		t.Fatal(err)
	}
	if savedName != name || savedPrice != price {
		t.Fatalf("snapshot changed: %q %d", savedName, savedPrice)
	}

	// Sinkron paralel: 12 transaksi berbeda masing-masing satu kopi mengurangi stok tepat 12 (boleh negatif).
	var wg sync.WaitGroup
	results := make(chan syncOutcome, 12)
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- send(newSale(uuid(), []saleItemInput{{ProductID: coffee.ID, ProductName: newName, Quantity: 1, UnitPrice: newPrice}}, 0))
		}()
	}
	wg.Wait()
	close(results)
	for out := range results {
		if out.Status != "accepted" {
			t.Fatalf("parallel sale: %+v", out)
		}
	}
	if got := stockOf(coffee.ID); got != -4 {
		t.Fatalf("coffee stock after parallel sales = %d, want -4", got)
	}

	// Retur sebagian lalu void sisanya: stok kembali tepat sekali, dan kunci idempotensi yang sama tidak mengulang.
	var coffeeItem string
	if err := s.DB.QueryRow(ctx, `SELECT i.id::text FROM sale_items i JOIN sales x ON x.id = i.sale_ref
		WHERE x.sale_id = $1 AND i.product_id = $2`, first.SaleID, coffee.ID).Scan(&coffeeItem); err != nil {
		t.Fatal(err)
	}
	// Koreksi memakai ID transaksi di server, bukan sale_id dari perangkat.
	serverSale := scalar(t, ctx, s, `SELECT id::text FROM sales WHERE sale_id = $1`, first.SaleID)
	ret := adjustmentInput{Type: "return", Reason: "Pelanggan batal satu gelas", Items: []adjustmentItemInput{{SaleItemID: coffeeItem, Quantity: 1}}}
	if _, created, err := s.adjustSale(ctx, owner, serverSale, "retur-1", ret); err != nil || !created {
		t.Fatalf("return: created=%v err=%v", created, err)
	}
	if _, created, err := s.adjustSale(ctx, owner, serverSale, "retur-1", ret); err != nil || created {
		t.Fatalf("return retry must replay: created=%v err=%v", created, err)
	}
	if got := stockOf(coffee.ID); got != -3 {
		t.Fatalf("coffee stock after return = %d, want -3", got)
	}

	// Lima void bersamaan dengan kunci berbeda: hanya satu yang berlaku.
	voidInput := adjustmentInput{Type: "void", Reason: "Salah input"}
	var successes int
	var mu sync.Mutex
	for i := range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, created, err := s.adjustSale(ctx, owner, serverSale, fmt.Sprintf("void-%d", i), voidInput)
			mu.Lock()
			defer mu.Unlock()
			if err == nil && created {
				successes++
			} else if err != nil && !errors.Is(err, errSaleVoided) && !errors.Is(err, errNothingToAdjust) {
				t.Errorf("void %d: %v", i, err)
			}
		}()
	}
	wg.Wait()
	if successes != 1 {
		t.Fatalf("concurrent voids applied %d times, want 1", successes)
	}
	if got := stockOf(coffee.ID); got != -2 {
		t.Fatalf("coffee stock after void = %d, want -2 (one remaining unit returned once)", got)
	}
	if got := stockOf(bread.ID); got != 10 {
		t.Fatalf("bread stock after void = %d, want 10", got)
	}
	var movements int64
	if err := s.DB.QueryRow(ctx, `SELECT COALESCE(sum(delta), 0) FROM inventory_movements WHERE product_id = $1`, coffee.ID).Scan(&movements); err != nil {
		t.Fatal(err)
	}
	if movements != stockOf(coffee.ID) {
		t.Fatalf("stock %d does not match movement ledger %d", stockOf(coffee.ID), movements)
	}
}

func uuid() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func scalar(t *testing.T, ctx context.Context, s Store, query string, args ...any) string {
	t.Helper()
	var value string
	if err := s.DB.QueryRow(ctx, query, args...).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func exec(t *testing.T, ctx context.Context, s Store, query string, args ...any) {
	t.Helper()
	if _, err := s.DB.Exec(ctx, query, args...); err != nil {
		t.Fatal(err)
	}
}

// migratedInventoryStore menjalankan semua migration di skema sementara yang dihapus setelah uji.
func migratedInventoryStore(t *testing.T, ctx context.Context) Store {
	t.Helper()
	databaseURL := os.Getenv("TENANTKU_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set TENANTKU_TEST_DATABASE_URL to run the PostgreSQL cashier test")
	}
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	schema := pgx.Identifier{"inventory_test_" + strings.ToLower(rand.Text())}.Sanitize()
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		defer conn.Close(cleanup)
		if _, err := conn.Exec(cleanup, "DROP SCHEMA IF EXISTS "+schema+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	s := Store{DB: pool}
	const directory = "../../migrations"
	files, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		// Pembatalan acara memakai fungsi berhak khusus yang terikat ke skema public.
		if !strings.HasSuffix(file.Name(), ".up.sql") || file.Name() == "00013_cancel_unpaid_event_bookings.up.sql" {
			continue
		}
		sql, err := os.ReadFile(filepath.Join(directory, file.Name()))
		if err != nil {
			t.Fatal(err)
		}
		exec(t, ctx, s, string(sql))
	}
	return s
}
