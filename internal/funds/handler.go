package funds

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"kitakarsa.com/tenantku/backend/internal/auth"
	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

// Routes registers the E09 endpoints. With no database they answer 503, like the other optional modules.
func (s Store) Routes(api *gin.RouterGroup, sessions *auth.Service, origin string) {
	mutations := api.Group("")
	if sessions != nil {
		mutations.Use(httpapi.RequireOrigin(origin))
	}
	guard := func(next func(*gin.Context, auth.Account)) func(*gin.Context, auth.Account) {
		return func(c *gin.Context, account auth.Account) {
			if s.DB == nil {
				httpapi.Error(c, http.StatusServiceUnavailable, "FUNDS_UNAVAILABLE", "Layanan dana belum tersedia.", nil)
				return
			}
			next(c, account)
		}
	}
	api.GET("/bookings/:id/funds", sessions.WithAccount(guard(s.bookingFunds)))
	mutations.POST("/bookings/:id/check-in-code", sessions.WithAccount(guard(s.checkInCode)))
	mutations.POST("/bookings/:id/disputes", sessions.WithAccount(guard(s.openDispute)))
	api.GET("/events/:id/check-ins", sessions.WithAccount(guard(s.listCheckIns)))
	mutations.POST("/events/:id/check-ins", sessions.WithAccount(guard(s.checkIn)))
	api.GET("/events/:id/funds", sessions.WithAccount(guard(s.organizerFunds)))
	api.GET("/events/:id/cancellation-preview", sessions.WithAccount(guard(s.cancellationPreview)))
	mutations.POST("/events/:id/payouts", sessions.WithAccount(guard(s.requestPayout)))
	api.GET("/organizations/:id/bank-account", sessions.WithAccount(guard(s.getBankAccount)))
	mutations.PUT("/organizations/:id/bank-account", sessions.WithAccount(guard(s.putBankAccount)))

	api.GET("/admin/funds", sessions.WithAccount(guard(s.adminQueue)))
	mutations.POST("/admin/refunds/:id/confirm", sessions.WithAccount(guard(s.confirmRefund)))
	mutations.POST("/admin/refunds/:id/fail", sessions.WithAccount(guard(s.failRefund)))
	mutations.POST("/admin/payouts/:id/release", sessions.WithAccount(guard(s.releasePayout)))
	mutations.POST("/admin/payouts/:id/fail", sessions.WithAccount(guard(s.failPayout)))
	mutations.POST("/admin/disputes/:id/resolve", sessions.WithAccount(guard(s.resolveDispute)))
}

func writeError(c *gin.Context, err error) {
	var blocked BlockedError
	var window WindowError
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		httpapi.Error(c, http.StatusNotFound, "NOT_FOUND", "Data tidak ditemukan.", nil)
	case errors.Is(err, ErrAdminRequired):
		httpapi.Error(c, http.StatusForbidden, "FORBIDDEN", "Hanya admin platform yang dapat melakukan aksi ini.", nil)
	case errors.Is(err, ErrInvalid):
		httpapi.Error(c, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Periksa isi permintaan.", nil)
	case errors.Is(err, ErrNotConfirmed):
		httpapi.Error(c, http.StatusConflict, "BOOKING_NOT_CONFIRMED", "Booking belum terkonfirmasi atau sudah dibatalkan.", nil)
	case errors.As(err, &window):
		httpapi.Error(c, http.StatusConflict, "CHECKIN_WINDOW_CLOSED", "Check-in hanya dibuka dari 2 jam sebelum acara sampai acara berakhir.",
			map[string]string{"opens_at": window.Opens.Format("2006-01-02T15:04:05Z07:00"), "closes_at": window.Closes.Format("2006-01-02T15:04:05Z07:00")})
	case errors.As(err, &blocked):
		httpapi.Error(c, http.StatusConflict, "PAYOUT_NOT_AVAILABLE", "Dana belum dapat dicairkan: "+strings.Join(blocked.Blockers, ", ")+".", nil)
	case errors.Is(err, ErrPayoutExists):
		httpapi.Error(c, http.StatusConflict, "PAYOUT_ALREADY_REQUESTED", "Pencairan untuk acara ini sudah diajukan.", nil)
	case errors.Is(err, ErrIdempotencyReuse):
		httpapi.Error(c, http.StatusConflict, "IDEMPOTENCY_PAYLOAD_MISMATCH", "Kunci ini sudah dipakai untuk permintaan lain.", nil)
	case errors.Is(err, ErrDisputeClosed):
		httpapi.Error(c, http.StatusConflict, "DISPUTE_CLOSED", "Dana acara ini sudah dicairkan, sengketa tidak dapat dibuka lagi.", nil)
	case errors.Is(err, ErrDisputeOpen):
		httpapi.Error(c, http.StatusConflict, "DISPUTE_ALREADY_OPEN", "Sudah ada sengketa yang sedang diproses untuk booking ini.", nil)
	case errors.Is(err, ErrAmountTooHigh):
		httpapi.Error(c, http.StatusConflict, "REFUND_AMOUNT_TOO_HIGH", "Jumlah melebihi sewa yang masih dapat dikembalikan.", nil)
	case errors.Is(err, ErrState):
		httpapi.Error(c, http.StatusConflict, "INVALID_STATE", "Status saat ini tidak mengizinkan aksi ini.", nil)
	default:
		httpapi.LogInternalError(c, "funds request failed", err)
		httpapi.Error(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Terjadi kesalahan. Coba lagi nanti.", nil)
	}
}

func (s Store) bookingFunds(c *gin.Context, a auth.Account) {
	data, err := s.BookingFundsFor(c.Request.Context(), a.ID, strings.ToLower(c.Param("id")))
	respond(c, http.StatusOK, data, err)
}

func (s Store) checkInCode(c *gin.Context, a auth.Account) {
	data, err := s.CheckInCodeFor(c.Request.Context(), a.ID, strings.ToLower(c.Param("id")))
	respond(c, http.StatusOK, data, err)
}

type disputeInput struct {
	Reason string `json:"reason"`
}

func (s Store) openDispute(c *gin.Context, a auth.Account) {
	input, ok := httpapi.ReadJSON[disputeInput](c, 8<<10)
	if !ok {
		return
	}
	data, err := s.OpenDispute(c.Request.Context(), a.ID, strings.ToLower(c.Param("id")), input.Reason)
	respond(c, http.StatusCreated, data, err)
}

func (s Store) listCheckIns(c *gin.Context, a auth.Account) {
	limit, err := strconv.Atoi(c.DefaultQuery("limit", "100"))
	if err != nil || limit < 1 || limit > 200 || len(c.Query("q")) > 100 {
		httpapi.Error(c, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Parameter tidak valid.", nil)
		return
	}
	data, err := s.ListCheckIns(c.Request.Context(), a.ID, strings.ToLower(c.Param("id")), strings.TrimSpace(c.Query("q")), limit)
	respond(c, http.StatusOK, data, err)
}

func (s Store) checkIn(c *gin.Context, a auth.Account) {
	input, ok := httpapi.ReadJSON[CheckInInput](c, 2<<10)
	if !ok {
		return
	}
	data, err := s.CheckIn(c.Request.Context(), a.ID, strings.ToLower(c.Param("id")), input)
	status := http.StatusCreated
	if err == nil && data.Already {
		status = http.StatusOK
	}
	respond(c, status, data, err)
}

func (s Store) organizerFunds(c *gin.Context, a auth.Account) {
	data, err := s.OrganizerFundsFor(c.Request.Context(), a.ID, strings.ToLower(c.Param("id")))
	respond(c, http.StatusOK, data, err)
}

func (s Store) cancellationPreview(c *gin.Context, a auth.Account) {
	data, err := s.PreviewCancellation(c.Request.Context(), a.ID, strings.ToLower(c.Param("id")))
	respond(c, http.StatusOK, data, err)
}

func (s Store) requestPayout(c *gin.Context, a auth.Account) {
	key := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if len(key) < 1 || len(key) > 128 {
		httpapi.Error(c, http.StatusBadRequest, "INVALID_IDEMPOTENCY_KEY", "Sertakan Idempotency-Key dengan panjang 1–128 byte.", nil)
		return
	}
	data, created, err := s.RequestPayout(c.Request.Context(), a.ID, strings.ToLower(c.Param("id")), key)
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	respond(c, status, data, err)
}

func (s Store) getBankAccount(c *gin.Context, a auth.Account) {
	data, err := s.GetBankAccount(c.Request.Context(), a.ID, strings.ToLower(c.Param("id")))
	respond(c, http.StatusOK, data, err)
}

func (s Store) putBankAccount(c *gin.Context, a auth.Account) {
	input, ok := httpapi.ReadJSON[BankAccount](c, 2<<10)
	if !ok {
		return
	}
	data, err := s.SetBankAccount(c.Request.Context(), a.ID, strings.ToLower(c.Param("id")), input)
	respond(c, http.StatusOK, data, err)
}

func (s Store) adminQueue(c *gin.Context, a auth.Account) {
	data, err := s.AdminQueueFor(c.Request.Context(), a.ID)
	respond(c, http.StatusOK, data, err)
}

type confirmInput struct {
	ProviderRef string `json:"provider_ref"`
	Fee         int64  `json:"fee"`
}

type reasonInput struct {
	Reason string `json:"reason"`
}

type resolveInput struct {
	Outcome string `json:"outcome"`
	Amount  int64  `json:"amount"`
	Note    string `json:"note"`
}

func (s Store) confirmRefund(c *gin.Context, a auth.Account) {
	input, ok := httpapi.ReadJSON[confirmInput](c, 2<<10)
	if !ok {
		return
	}
	data, err := s.ConfirmRefund(c.Request.Context(), a.ID, strings.ToLower(c.Param("id")), input.ProviderRef, input.Fee)
	respond(c, http.StatusOK, data, err)
}

func (s Store) failRefund(c *gin.Context, a auth.Account) {
	input, ok := httpapi.ReadJSON[reasonInput](c, 2<<10)
	if !ok {
		return
	}
	data, err := s.FailRefund(c.Request.Context(), a.ID, strings.ToLower(c.Param("id")), input.Reason)
	respond(c, http.StatusOK, data, err)
}

func (s Store) releasePayout(c *gin.Context, a auth.Account) {
	input, ok := httpapi.ReadJSON[confirmInput](c, 2<<10)
	if !ok {
		return
	}
	data, err := s.ReleasePayout(c.Request.Context(), a.ID, strings.ToLower(c.Param("id")), input.ProviderRef)
	respond(c, http.StatusOK, data, err)
}

func (s Store) failPayout(c *gin.Context, a auth.Account) {
	input, ok := httpapi.ReadJSON[reasonInput](c, 2<<10)
	if !ok {
		return
	}
	data, err := s.FailPayout(c.Request.Context(), a.ID, strings.ToLower(c.Param("id")), input.Reason)
	respond(c, http.StatusOK, data, err)
}

func (s Store) resolveDispute(c *gin.Context, a auth.Account) {
	input, ok := httpapi.ReadJSON[resolveInput](c, 4<<10)
	if !ok {
		return
	}
	dispute, refund, err := s.ResolveDispute(c.Request.Context(), a.ID, strings.ToLower(c.Param("id")), input.Outcome, input.Amount, input.Note)
	respond(c, http.StatusOK, gin.H{"dispute": dispute, "refund": refund}, err)
}

func respond(c *gin.Context, status int, data any, err error) {
	if err != nil {
		writeError(c, err)
		return
	}
	httpapi.Success(c, status, data)
}
