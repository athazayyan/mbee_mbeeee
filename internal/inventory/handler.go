package inventory

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"kitakarsa.com/tenantku/backend/internal/auth"
	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

const (
	maxInput     = 16 << 10
	maxRupiah    = 1<<53 - 1
	maxStock     = 1_000_000
	maxStockStep = 100_000
)

var skuPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,39}$`)

type fieldErrors map[string]string

func (fieldErrors) Error() string { return "product validation failed" }

func (s Store) Routes(api *gin.RouterGroup, sessions *auth.Service, origin string) {
	api.GET("/businesses/:id/products", sessions.WithAccount(s.listProducts))
	api.GET("/businesses/:id/sales", sessions.WithAccount(s.listSalesHandler))
	mutations := api.Group("")
	if sessions != nil {
		mutations.Use(httpapi.RequireOrigin(origin))
	}
	mutations.POST("/businesses/:id/products", sessions.WithAccount(s.createProduct))
	mutations.PATCH("/products/:id", sessions.WithAccount(s.updateProduct))
	mutations.POST("/products/:id/stock-movements", sessions.WithAccount(s.moveStock))
	mutations.POST("/businesses/:id/pos-sessions", sessions.WithAccount(s.openPosSession))
	mutations.POST("/sync/sales", sessions.WithAccount(s.syncSales))
}

func validRupiah(value *int64) bool { return value == nil || (*value >= 0 && *value <= maxRupiah) }

// validateProduct memeriksa isian. create mewajibkan nama dan harga; update minimal satu perubahan.
func validateProduct(input *productInput, create bool) fieldErrors {
	fields := make(fieldErrors)
	if input.Name.Set {
		if input.Name.Value == nil {
			fields["name"] = "Isi nama produk."
		} else {
			name := strings.TrimSpace(*input.Name.Value)
			input.Name.Value = &name
			if !httpapi.ValidSingleLine(name, 120) {
				fields["name"] = "Isi nama produk satu baris hingga 120 karakter."
			}
		}
	} else if create {
		fields["name"] = "Isi nama produk."
	}
	if input.SKU.Set && input.SKU.Value != nil {
		sku := strings.TrimSpace(*input.SKU.Value)
		if sku == "" {
			input.SKU.Value = nil
		} else if !skuPattern.MatchString(sku) {
			fields["sku"] = "Gunakan SKU hingga 40 karakter berupa huruf, angka, titik, garis bawah, atau tanda hubung."
		} else {
			input.SKU.Value = &sku
		}
	}
	if (create && !input.Price.Set) || (input.Price.Set && input.Price.Value == nil) {
		fields["price"] = "Isi harga jual."
	} else if !validRupiah(input.Price.Value) {
		fields["price"] = "Harga jual harus rupiah bulat dan tidak negatif."
	}
	if !validRupiah(input.Cost.Value) {
		fields["cost"] = "HPP harus rupiah bulat dan tidak negatif."
	}
	if input.CategoryID.Value != nil {
		id := strings.ToLower(*input.CategoryID.Value)
		input.CategoryID.Value = &id
		if !httpapi.ValidID(id) {
			fields["category_id"] = "Pilih kategori yang valid."
		}
	}
	if input.PhotoUploadID.Value != nil {
		fields["photo_upload_id"] = "Unggah foto produk belum tersedia."
	}
	if input.InitialStock.Set {
		switch {
		case !create:
			fields["initial_stock"] = "Ubah stok lewat mutasi stok."
		case input.InitialStock.Value == nil || *input.InitialStock.Value < 0 || *input.InitialStock.Value > maxStock:
			fields["initial_stock"] = "Stok awal harus 0 sampai 1.000.000."
		}
	}
	if input.Archived.Set && (create || input.Archived.Value == nil) {
		fields["archived"] = "Status arsip hanya bisa diubah pada produk yang sudah ada."
	}
	if !create && !input.Name.Set && !input.SKU.Set && !input.Price.Set && !input.Cost.Set &&
		!input.CategoryID.Set && !input.Archived.Set {
		fields["name"] = "Isi setidaknya satu perubahan."
	}
	if len(fields) == 0 {
		return nil
	}
	return fields
}

func validateStock(input *stockInput) fieldErrors {
	fields := make(fieldErrors)
	switch input.Reason {
	case "restock":
		if input.Delta <= 0 || input.Delta > maxStockStep {
			fields["delta"] = "Penambahan stok harus 1 sampai 100.000."
		}
	case "correction":
		if input.Delta == 0 || input.Delta > maxStockStep || input.Delta < -maxStockStep {
			fields["delta"] = "Koreksi stok harus selain nol dan paling banyak 100.000."
		}
	default:
		fields["reason"] = "Pilih alasan restock atau correction."
	}
	if input.Note != nil {
		note := strings.TrimSpace(*input.Note)
		if note == "" {
			input.Note = nil
		} else if !httpapi.ValidSingleLine(note, 200) {
			fields["note"] = "Catatan satu baris hingga 200 karakter."
		} else {
			input.Note = &note
		}
	}
	if len(fields) == 0 {
		return nil
	}
	return fields
}

func validID(c *gin.Context, id string) bool {
	if httpapi.ValidID(id) {
		return true
	}
	writeError(c, pgx.ErrNoRows)
	return false
}

func (s Store) listProducts(c *gin.Context, account auth.Account) {
	business := c.Param("id")
	if !validID(c, business) {
		return
	}
	fields := make(fieldErrors)
	limit, err := strconv.Atoi(c.DefaultQuery("limit", "50"))
	if err != nil || limit < 1 || limit > 100 {
		fields["limit"] = "Gunakan limit antara 1 dan 100."
	}
	query := strings.TrimSpace(c.Query("q"))
	if query != "" && !httpapi.ValidSingleLine(query, 120) {
		fields["q"] = "Gunakan kata kunci satu baris hingga 120 karakter."
	}
	includeArchived := c.Query("include_archived") == "true"
	after, afterID, err := httpapi.DecodeCursor(c.Query("cursor"))
	if err != nil {
		fields["cursor"] = "Gunakan cursor dari halaman sebelumnya."
	}
	if len(fields) > 0 {
		writeError(c, fields)
		return
	}
	data, err := s.list(c.Request.Context(), account.ID, business, listFilter{
		Query: query, IncludeArchived: includeArchived, AfterAt: after, AfterID: afterID, Limit: limit + 1,
	})
	if err != nil {
		writeError(c, err)
		return
	}
	var next *string
	if len(data) > limit {
		data = data[:limit]
		last := data[len(data)-1]
		cursor := httpapi.EncodeCursor(last.CreatedAt, last.ID)
		next = &cursor
	}
	if data == nil {
		data = []Product{}
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{"next_cursor": next}})
}

func (s Store) createProduct(c *gin.Context, account auth.Account) {
	business := c.Param("id")
	if !validID(c, business) {
		return
	}
	input, ok := httpapi.ReadJSON[productInput](c, maxInput)
	if !ok {
		return
	}
	if fields := validateProduct(&input, true); fields != nil {
		writeError(c, fields)
		return
	}
	product, err := s.create(c.Request.Context(), account.ID, business, input)
	writeProduct(c, http.StatusCreated, product, err)
}

func (s Store) updateProduct(c *gin.Context, account auth.Account) {
	id := c.Param("id")
	if !validID(c, id) {
		return
	}
	input, ok := httpapi.ReadJSON[productInput](c, maxInput)
	if !ok {
		return
	}
	if fields := validateProduct(&input, false); fields != nil {
		writeError(c, fields)
		return
	}
	product, err := s.update(c.Request.Context(), account.ID, id, input)
	writeProduct(c, http.StatusOK, product, err)
}

func (s Store) moveStock(c *gin.Context, account auth.Account) {
	id := c.Param("id")
	if !validID(c, id) {
		return
	}
	input, ok := httpapi.ReadJSON[stockInput](c, maxInput)
	if !ok {
		return
	}
	if fields := validateStock(&input); fields != nil {
		writeError(c, fields)
		return
	}
	product, err := s.adjustStock(c.Request.Context(), account.ID, id, input)
	writeProduct(c, http.StatusOK, product, err)
}

func writeProduct(c *gin.Context, status int, data Product, err error) {
	if err != nil {
		writeError(c, err)
		return
	}
	httpapi.Success(c, status, data)
}

func writeError(c *gin.Context, err error) {
	var fields fieldErrors
	var dbError *pgconn.PgError
	switch {
	case errors.As(err, &fields):
		httpapi.Error(c, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Periksa isian.", fields)
	case errors.Is(err, pgx.ErrNoRows):
		httpapi.Error(c, http.StatusNotFound, "NOT_FOUND", "Data atau usaha tidak ditemukan.", nil)
	case errors.Is(err, errStockOverflow):
		httpapi.Error(c, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Periksa isian.",
			map[string]string{"delta": "Stok produk harus tetap di antara -1.000.000 dan 1.000.000."})
	case errors.As(err, &dbError) && dbError.ConstraintName == "products_business_sku_idx":
		httpapi.Error(c, http.StatusConflict, "SKU_IN_USE", "SKU sudah dipakai produk lain di usaha ini.",
			map[string]string{"sku": "Gunakan SKU lain."})
	case errors.As(err, &dbError) && dbError.ConstraintName == "products_category_id_fkey":
		httpapi.Error(c, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Periksa isian.",
			map[string]string{"category_id": "Pilih kategori yang tersedia."})
	default:
		httpapi.LogInternalError(c, "product request failed", err)
		httpapi.Error(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Produk belum dapat diproses.", nil)
	}
}
