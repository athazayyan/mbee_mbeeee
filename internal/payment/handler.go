package payment

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"kitakarsa.com/tenantku/backend/internal/auth"
	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

var methodCode = regexp.MustCompile(`^[a-z0-9_]{1,40}$`)

func (s Store) Routes(api *gin.RouterGroup, sessions *auth.Service, origin string) {
	api.POST("/bookings/:id/payments", httpapi.RequireOrigin(origin), sessions.WithAccount(s.createPayment))
	api.GET("/bookings/:id/payment-quotes", sessions.WithAccount(s.listBookingPaymentQuotes))
	api.GET("/bookings/:id/payment-options", sessions.WithAccount(s.paymentOptions))
	api.POST("/webhooks/payments/:provider", s.webhook)
	s.planRoutes(api, sessions, origin)
	s.simulationRoutes(api, sessions, origin)
}

func (s Store) createPayment(c *gin.Context, account auth.Account) {
	if s.Provider == nil || s.DB == nil {
		httpapi.Error(c, http.StatusServiceUnavailable, "PAYMENT_UNAVAILABLE", "Pembayaran belum tersedia.", nil)
		return
	}
	id, key := c.Param("id"), c.GetHeader("Idempotency-Key")
	if len(key) < 1 || len(key) > 128 {
		httpapi.Error(c, http.StatusBadRequest, "INVALID_IDEMPOTENCY_KEY", "Sertakan Idempotency-Key dengan panjang 1–128 byte.", nil)
		return
	}
	input, ok := httpapi.ReadJSON[Input](c, 2048)
	if !ok {
		return
	}
	input.Method = strings.ToLower(strings.TrimSpace(input.Method))
	if !methodCode.MatchString(input.Method) || len([]rune(input.Description)) > 200 {
		httpapi.Error(c, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Pilih metode pembayaran.",
			map[string]string{"method": "Pilih salah satu metode yang tersedia."})
		return
	}
	if !httpapi.ValidID(id) {
		httpapi.Error(c, http.StatusNotFound, "NOT_FOUND", "Booking tidak ditemukan.", nil)
		return
	}
	attempt, created, err := s.CreateAttempt(c.Request.Context(), account.ID, account.Email, id, key, input)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		httpapi.Error(c, http.StatusNotFound, "NOT_FOUND", "Booking tidak ditemukan.", nil)
	case errors.Is(err, ErrUnknownMethod):
		httpapi.Error(c, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Metode pembayaran tidak tersedia.",
			map[string]string{"method": "Pilih salah satu metode yang tersedia."})
	case errors.Is(err, ErrQuoteChanged):
		httpapi.Error(c, http.StatusConflict, "PAYMENT_QUOTE_CHANGED", "Muat ulang rincian biaya lalu konfirmasi total pembayaran.", nil)
	case errors.Is(err, ErrPaymentInProgress):
		httpapi.Error(c, http.StatusConflict, "PAYMENT_ALREADY_OPEN", "Pembayaran sebelumnya masih diproses. Tunggu statusnya berubah sebelum mengganti metode.", nil)
	case errors.Is(err, ErrNotPayable), errors.Is(err, ErrAmountTooLarge):
		httpapi.Error(c, http.StatusConflict, "BOOKING_NOT_PAYABLE", "Booking sudah tidak dapat dibayar.", nil)
	case errors.Is(err, ErrIdempotencyMismatch):
		httpapi.Error(c, http.StatusConflict, "IDEMPOTENCY_PAYLOAD_MISMATCH", "Kunci ini sudah dipakai untuk permintaan lain.", nil)
	case errors.Is(err, ErrProvider):
		httpapi.LogInternalError(c, "payment provider failed", err)
		httpapi.Error(c, http.StatusServiceUnavailable, "PAYMENT_PROVIDER_UNAVAILABLE", "Penyedia pembayaran sedang tidak tersedia. Coba lagi.", nil)
	case err != nil:
		httpapi.LogInternalError(c, "payment attempt failed", err)
		httpapi.Error(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Pembayaran belum dapat dibuat.", nil)
	default:
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		c.JSON(status, gin.H{"data": attempt, "meta": gin.H{}})
	}
}

func (s Store) paymentOptions(c *gin.Context, account auth.Account) {
	if s.DB == nil {
		httpapi.Error(c, http.StatusServiceUnavailable, "PAYMENT_UNAVAILABLE", "Pembayaran belum tersedia.", nil)
		return
	}
	id := c.Param("id")
	if !httpapi.ValidID(id) {
		httpapi.Error(c, http.StatusNotFound, "NOT_FOUND", "Booking tidak ditemukan.", nil)
		return
	}
	options, err := s.PaymentOptions(c.Request.Context(), account.ID, strings.ToLower(id))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		httpapi.Error(c, http.StatusNotFound, "NOT_FOUND", "Booking tidak ditemukan.", nil)
	case err != nil:
		httpapi.LogInternalError(c, "payment options failed", err)
		httpapi.Error(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Pilihan pembayaran belum dapat dimuat.", nil)
	default:
		c.JSON(http.StatusOK, gin.H{"data": options, "meta": gin.H{}})
	}
}

// webhook acknowledges only after the event is durably stored. Failures return 5xx so Midtrans retries.
func (s Store) webhook(c *gin.Context) {
	if c.Param("provider") != "midtrans" {
		httpapi.Error(c, http.StatusNotFound, "NOT_FOUND", "Penyedia tidak dikenal.", nil)
		return
	}
	if s.Provider == nil || s.DB == nil {
		httpapi.Error(c, http.StatusServiceUnavailable, "PAYMENT_UNAVAILABLE", "Pembayaran belum tersedia.", nil)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10))
	var n Notification
	if err != nil || json.Unmarshal(raw, &n) != nil || !httpapi.ValidID(n.OrderID) || n.SignatureKey == "" {
		httpapi.Error(c, http.StatusBadRequest, "INVALID_WEBHOOK", "Payload webhook tidak valid.", nil)
		return
	}
	if !s.Provider.ValidSignature(n) {
		httpapi.Error(c, http.StatusUnauthorized, "INVALID_SIGNATURE", "Tanda tangan tidak valid.", nil)
		return
	}
	verified, err := s.Provider.Status(c.Request.Context(), n.OrderID)
	if err != nil {
		httpapi.LogInternalError(c, "payment status verification failed", err)
		httpapi.Error(c, http.StatusServiceUnavailable, "PAYMENT_PROVIDER_UNAVAILABLE", "Status pembayaran belum dapat diverifikasi.", nil)
		return
	}
	payload, err := json.Marshal(verified)
	if err != nil {
		httpapi.Error(c, http.StatusServiceUnavailable, "PAYMENT_UNAVAILABLE", "Webhook belum dapat diproses.", nil)
		return
	}
	if err := s.ApplyNotification(c.Request.Context(), verified, payload); err != nil {
		httpapi.LogInternalError(c, "payment webhook failed", err)
		httpapi.Error(c, http.StatusServiceUnavailable, "PAYMENT_UNAVAILABLE", "Webhook belum dapat diproses.", nil)
		return
	}
	c.Status(http.StatusNoContent)
}
