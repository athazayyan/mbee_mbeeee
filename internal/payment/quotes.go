package payment

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"kitakarsa.com/tenantku/backend/internal/auth"
	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

type bookingPaymentQuote struct {
	Method string `json:"method"`
	Rent   int64  `json:"rent"`
	Admin  int64  `json:"admin"`
	Total  int64  `json:"total"`
}

func (s Store) listBookingPaymentQuotes(c *gin.Context, account auth.Account) {
	if s.DB == nil || s.Provider == nil {
		httpapi.Error(c, http.StatusServiceUnavailable, "PAYMENT_UNAVAILABLE", "Pembayaran belum tersedia.", nil)
		return
	}
	id := c.Param("id")
	if !httpapi.ValidID(id) {
		httpapi.Error(c, http.StatusNotFound, "NOT_FOUND", "Booking tidak ditemukan.", nil)
		return
	}
	var rent int64
	var payable bool
	err := s.DB.QueryRow(c.Request.Context(), `SELECT b.rent,
		b.status IN ('held', 'payment_pending') AND b.expires_at > clock_timestamp() AND e.status IN ('published', 'live')
		FROM bookings b JOIN events e ON e.id = b.event_id
		WHERE b.id = $1 AND (b.created_by = $2 OR EXISTS (
			SELECT 1 FROM memberships m WHERE m.user_id = $2 AND m.role = 'owner' AND m.business_id = b.business_id))`,
		id, account.ID).Scan(&rent, &payable)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		httpapi.Error(c, http.StatusNotFound, "NOT_FOUND", "Booking tidak ditemukan.", nil)
	case err != nil:
		httpapi.LogInternalError(c, "booking payment quotes failed", err)
		httpapi.Error(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Rincian pembayaran belum dapat dimuat.", nil)
	case !payable || rent < 1 || rent > 9007199254736991:
		httpapi.Error(c, http.StatusConflict, "BOOKING_NOT_PAYABLE", "Booking sudah tidak dapat dibayar.", nil)
	default:
		quotes := make([]bookingPaymentQuote, 0, 2)
		for _, method := range []string{"bca_va", "other_qris"} {
			admin, _ := bookingAdmin(method)
			quotes = append(quotes, bookingPaymentQuote{Method: method, Rent: rent, Admin: admin, Total: rent + admin})
		}
		httpapi.Success(c, http.StatusOK, quotes)
	}
}
