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
