package booking

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"kitakarsa.com/tenantku/backend/internal/auth"
	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

func (s Store) Routes(api *gin.RouterGroup, sessions *auth.Service) {
	api.GET("/bookings", sessions.WithAccount(s.listBookings))
	api.GET("/bookings/:id", sessions.WithAccount(s.getBooking))
}

// HoldRoutes is registered when the runtime has a stand, price, and policy source.
func (s Store) HoldRoutes(api *gin.RouterGroup, sessions *auth.Service, origin string, settings HoldSettings, source OfferSource) error {
	if s.DB == nil || sessions == nil || origin == "" || !settings.valid(source) {
		return ErrHoldUnavailable
	}
	api.POST("/bookings/holds", httpapi.RequireOrigin(origin), sessions.WithAccount(func(c *gin.Context, account auth.Account) {
		key := c.GetHeader("Idempotency-Key")
		if !validHoldKey(key) {
			httpapi.Error(c, http.StatusBadRequest, "INVALID_IDEMPOTENCY_KEY", "Sertakan Idempotency-Key dengan panjang 1–128 byte.", nil)
			return
		}
		input, ok := httpapi.ReadJSON[HoldInput](c, 4096)
		if !ok {
			return
		}
		reply, created, err := s.createHold(c.Request.Context(), account.ID, key, input, settings, source)
		if err != nil {
			writeHoldError(c, err)
			return
		}
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		c.JSON(status, gin.H{"data": reply, "meta": gin.H{"server_now": reply.ServerNow}})
	}))
	api.POST("/bookings/hold-quote", httpapi.RequireOrigin(origin), sessions.WithAccount(func(c *gin.Context, account auth.Account) {
		input, ok := httpapi.ReadJSON[holdQuoteInput](c, 4096)
		if !ok {
			return
		}
		quote, err := s.quoteHold(c.Request.Context(), account.ID, input, source)
		if err != nil {
			writeHoldError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": quote, "meta": gin.H{"server_now": quote.ServerNow}})
	}))
	return nil
}

func writeHoldError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		httpapi.Error(c, http.StatusNotFound, "NOT_FOUND", "Usaha, acara, atau slot tidak tersedia.", nil)
	case errors.Is(err, ErrHoldInput):
		httpapi.Error(c, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Periksa usaha, acara, stand, slot, dan versi kebijakan.", nil)
	case errors.Is(err, ErrIdempotencyMismatch):
		httpapi.Error(c, http.StatusConflict, "IDEMPOTENCY_PAYLOAD_MISMATCH", "Kunci ini sudah dipakai untuk pengajuan lain.", nil)
	case errors.Is(err, ErrNotBookable):
		httpapi.Error(c, http.StatusConflict, "EVENT_NOT_BOOKABLE", "Stand atau slot belum dapat dipesan.", nil)
	case errors.Is(err, ErrStandUnavailable):
		httpapi.Error(c, http.StatusConflict, "STAND_UNAVAILABLE", "Stand sudah dialokasikan pada slot ini.", nil)
	case errors.Is(err, ErrQuotaFull):
		httpapi.Error(c, http.StatusConflict, "CATEGORY_QUOTA_FULL", "Kuota kategori usaha sudah penuh.", nil)
	case errors.Is(err, ErrPolicyChanged):
		httpapi.Error(c, http.StatusConflict, "REFUND_POLICY_CHANGED", "Muat ulang dan tinjau kebijakan refund terbaru.", nil)
	case errors.Is(err, ErrHoldPriceChanged):
		httpapi.Error(c, http.StatusConflict, "HOLD_PRICE_CHANGED", "Harga berubah. Tinjau lagi rincian biaya sebelum membuat hold.", nil)
	case errors.Is(err, ErrHoldLimit):
		httpapi.Error(c, http.StatusTooManyRequests, "HOLD_LIMIT_REACHED", "Batas hold aktif tercapai.", nil)
	case errors.Is(err, ErrHoldUnavailable):
		httpapi.Error(c, http.StatusServiceUnavailable, "HOLD_UNAVAILABLE", "Layanan hold belum tersedia.", nil)
	default:
		httpapi.LogInternalError(c, "booking hold failed", err)
		httpapi.Error(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Hold belum dapat dibuat.", nil)
	}
}

func (s Store) getBooking(c *gin.Context, account auth.Account) {
	id := c.Param("id")
	if !httpapi.ValidID(id) {
		writeReadError(c, pgx.ErrNoRows)
		return
	}
	reply, err := s.detail(c.Request.Context(), account.ID, id)
	if err != nil {
		writeReadError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": reply, "meta": gin.H{"server_now": reply.ServerNow}})
}

func (s Store) listBookings(c *gin.Context, account auth.Account) {
	business := c.Query("business_id")
	if !httpapi.ValidID(business) {
		httpapi.Error(c, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Pilih usaha yang valid.", map[string]string{"business_id": "ID usaha diperlukan."})
		return
	}
	limit, err := strconv.Atoi(c.DefaultQuery("limit", "20"))
	if err != nil || limit < 1 || limit > 100 {
		httpapi.Error(c, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Parameter tidak valid.", map[string]string{"limit": "Gunakan limit antara 1 dan 100."})
		return
	}
	before, id, err := httpapi.DecodeCursor(c.Query("cursor"))
	if err != nil {
		httpapi.Error(c, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Parameter tidak valid.", map[string]string{"cursor": "Gunakan cursor dari halaman sebelumnya."})
		return
	}
	var beforeID *string
	if id != "" {
		beforeID = &id
	}
	bookings, now, err := s.list(c.Request.Context(), account.ID, business, before, beforeID, limit+1)
	if err != nil {
		writeReadError(c, err)
		return
	}
	var next *string
	if len(bookings) > limit {
		bookings = bookings[:limit]
		last := bookings[len(bookings)-1]
		cursor := httpapi.EncodeCursor(last.CreatedAt, last.ID)
		next = &cursor
	}
	c.JSON(http.StatusOK, gin.H{"data": bookings, "meta": gin.H{"server_now": now, "next_cursor": next}})
}

func writeReadError(c *gin.Context, err error) {
	if errors.Is(err, pgx.ErrNoRows) {
		httpapi.Error(c, http.StatusNotFound, "NOT_FOUND", "Booking atau usaha tidak ditemukan.", nil)
		return
	}
	httpapi.LogInternalError(c, "booking read failed", err)
	httpapi.Error(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Booking belum dapat dimuat.", nil)
}
