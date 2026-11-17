package booking

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"kitakarsa.com/tenantku/backend/internal/auth"
	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

type reasonInput struct {
	Reason string `json:"reason"`
}

type registrationInput struct {
	Mode string `json:"mode"`
}

// ApplicationRoutes serves curated registration. offerLifetime is how long an approved tenant has to pay.
func (s Store) ApplicationRoutes(api *gin.RouterGroup, sessions *auth.Service, origin string, offerLifetime time.Duration, maxActive int, source OfferSource) error {
	if s.DB == nil || sessions == nil || origin == "" || source == nil || offerLifetime < time.Second || maxActive < 1 {
		return ErrHoldUnavailable
	}
	guard := httpapi.RequireOrigin(origin)
	api.POST("/events/:id/applications", guard, sessions.WithAccount(func(c *gin.Context, account auth.Account) {
		input, ok := httpapi.ReadJSON[ApplicationInput](c, 4096)
		if !ok {
			return
		}
		result, err := s.Submit(c.Request.Context(), account.ID, c.Param("id"), input)
		if err != nil {
			writeApplicationError(c, err)
			return
		}
		c.JSON(http.StatusCreated, gin.H{"data": result, "meta": gin.H{}})
	}))
	api.GET("/events/:id/applications", sessions.WithAccount(func(c *gin.Context, account auth.Account) {
		limit, ok := applicationLimit(c)
		if !ok {
			return
		}
		result, err := s.ListForEvent(c.Request.Context(), account.ID, c.Param("id"), limit)
		if err != nil {
			writeApplicationError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": result, "meta": gin.H{}})
	}))
	api.GET("/applications", sessions.WithAccount(func(c *gin.Context, account auth.Account) {
		limit, ok := applicationLimit(c)
		if !ok {
			return
		}
		result, err := s.ListForBusiness(c.Request.Context(), account.ID, c.Query("business_id"), limit)
		if err != nil {
			writeApplicationError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": result, "meta": gin.H{}})
	}))
	api.POST("/applications/:id/approve", guard, sessions.WithAccount(func(c *gin.Context, account auth.Account) {
		input, ok := httpapi.ReadJSON[ApproveInput](c, 4096)
		if !ok {
			return
		}
		result, err := s.Approve(c.Request.Context(), account.ID, c.Param("id"), input, offerLifetime, maxActive, source)
		if err != nil {
			writeApplicationError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": result, "meta": gin.H{}})
	}))
	api.POST("/applications/:id/reject", guard, sessions.WithAccount(func(c *gin.Context, account auth.Account) {
		input, ok := httpapi.ReadJSON[reasonInput](c, 4096)
		if !ok {
			return
		}
		result, err := s.Reject(c.Request.Context(), account.ID, c.Param("id"), input.Reason)
		if err != nil {
			writeApplicationError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": result, "meta": gin.H{}})
	}))
	api.GET("/events/:id/registration-mode", guard, sessions.WithAccount(func(c *gin.Context, account auth.Account) {
		mode, err := s.RegistrationMode(c.Request.Context(), account.ID, c.Param("id"))
		if err != nil {
			writeApplicationError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"mode": mode}, "meta": gin.H{}})
	}))
	api.PUT("/events/:id/registration-mode", guard, sessions.WithAccount(func(c *gin.Context, account auth.Account) {
		input, ok := httpapi.ReadJSON[registrationInput](c, 1024)
		if !ok {
			return
		}
		if err := s.SetRegistrationMode(c.Request.Context(), account.ID, c.Param("id"), input.Mode); err != nil {
			writeApplicationError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"mode": input.Mode}, "meta": gin.H{}})
	}))
	return nil
}

func applicationLimit(c *gin.Context) (int, bool) {
	limit, err := strconv.Atoi(c.DefaultQuery("limit", "50"))
	if err != nil || limit < 1 || limit > 100 {
		httpapi.Error(c, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Parameter tidak valid.",
			map[string]string{"limit": "Gunakan limit antara 1 dan 100."})
		return 0, false
	}
	return limit, true
}

func writeApplicationError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		httpapi.Error(c, http.StatusNotFound, "NOT_FOUND", "Acara, usaha, atau pengajuan tidak ditemukan.", nil)
	case errors.Is(err, ErrApplicationInput):
		httpapi.Error(c, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Periksa isi permintaan.", nil)
	case errors.Is(err, ErrNotCurated):
		httpapi.Error(c, http.StatusConflict, "EVENT_NOT_CURATED", "Acara ini memakai pemesanan langsung.", nil)
	case errors.Is(err, ErrApplicationDup):
		httpapi.Error(c, http.StatusConflict, "APPLICATION_EXISTS", "Usaha ini sudah memiliki pengajuan aktif untuk acara ini.", nil)
	case errors.Is(err, ErrApplicantBusy):
		httpapi.Error(c, http.StatusConflict, "APPLICANT_HAS_ACTIVE_BOOKING", "Pemohon masih punya booking aktif di acara ini. Setujui setelah booking itu selesai atau kedaluwarsa.", nil)
	case errors.Is(err, ErrApplicationState):
		httpapi.Error(c, http.StatusConflict, "APPLICATION_STATE", "Status pengajuan atau acara tidak memungkinkan aksi ini.", nil)
	default:
		writeHoldError(c, err) // stand, quota, price, and availability conflicts share hold semantics
	}
}
