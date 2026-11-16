package payment

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"encoding/json"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"kitakarsa.com/tenantku/backend/internal/auth"
	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

var methods = map[string]bool{"snap": true}

func (s Store) Routes(api *gin.RouterGroup, sessions *auth.Service, origin string) {
	api.POST("/bookings/:id/payments", httpapi.RequireOrigin(origin), sessions.WithAccount(s.createPayment))
	api.POST("/webhooks/payments/:provider", s.webhook)
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
	if !methods[input.Method] || len([]rune(input.Description)) > 200 {
		httpapi.Error(c, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Metode pembayaran tidak didukung.",
			map[string]string{"method": "Gunakan snap."})
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
	case errors.Is(err, ErrNotPayable):
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
	if err != nil || json.Unmarshal(raw, &n) != nil || n.OrderID == "" || n.SignatureKey == "" {
		httpapi.Error(c, http.StatusBadRequest, "INVALID_WEBHOOK", "Payload webhook tidak valid.", nil)
		return
	}
	if !s.Provider.ValidSignature(n) {
		httpapi.Error(c, http.StatusUnauthorized, "INVALID_SIGNATURE", "Tanda tangan tidak valid.", nil)
		return
	}
	if err := s.ApplyNotification(c.Request.Context(), n, raw); err != nil {
		httpapi.LogInternalError(c, "payment webhook failed", err)
		httpapi.Error(c, http.StatusServiceUnavailable, "PAYMENT_UNAVAILABLE", "Webhook belum dapat diproses.", nil)
		return
	}
	c.Status(http.StatusNoContent)
}
