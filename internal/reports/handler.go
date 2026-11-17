package reports

import (
	"errors"
	"net/http"
	"strings"
	"time"
	_ "time/tzdata" // Zona waktu tersedia juga di Windows dan image minimal.

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"kitakarsa.com/tenantku/backend/internal/auth"
	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

const (
	maxInput        = 4 << 10
	maxPeriod       = 366 * 24 * time.Hour
	defaultShareTTL = 7 * 24 * time.Hour
	maxShareTTL     = 30 * 24 * time.Hour
	defaultTimezone = "Asia/Jakarta"
)

type fieldErrors map[string]string

func (fieldErrors) Error() string { return "report validation failed" }

// Routes memasang rute rapor. origin adalah alamat app, dipakai untuk membentuk URL tautan berbagi.
func (s Store) Routes(api *gin.RouterGroup, sessions *auth.Service, origin string) {
	api.GET("/businesses/:id/reports", sessions.WithAccount(s.listReports))
	api.GET("/reports/:id", sessions.WithAccount(s.getReport))
	api.GET("/jobs/:id", sessions.WithAccount(s.getJob))
	api.GET("/reports/:id/shares", sessions.WithAccount(s.listShares))
	api.GET("/public/reports/:token", s.getPublic)
	mutations := api.Group("")
	if sessions != nil {
		mutations.Use(httpapi.RequireOrigin(origin))
	}
	mutations.POST("/businesses/:id/reports", sessions.WithAccount(s.requestReport))
	mutations.POST("/reports/:id/shares", sessions.WithAccount(func(c *gin.Context, account auth.Account) {
		s.postShare(c, account, origin)
	}))
	mutations.DELETE("/report-shares/:id", sessions.WithAccount(s.deleteShare))
}

func validateReport(input *reportInput, now time.Time) fieldErrors {
	fields := make(fieldErrors)
	if input.PeriodStart.IsZero() || input.PeriodEnd.IsZero() || !input.PeriodEnd.After(input.PeriodStart) {
		fields["period_end"] = "Akhir periode harus setelah awal periode."
	} else if input.PeriodEnd.Sub(input.PeriodStart) > maxPeriod {
		fields["period_end"] = "Periode rapor paling panjang satu tahun."
	} else if input.PeriodStart.After(now) {
		fields["period_start"] = "Awal periode tidak boleh di masa depan."
	}
	input.PeriodStart, input.PeriodEnd = input.PeriodStart.UTC(), input.PeriodEnd.UTC()
	if input.Timezone == "" {
		input.Timezone = defaultTimezone
	}
	if _, err := time.LoadLocation(input.Timezone); err != nil || !strings.Contains(input.Timezone, "/") {
		fields["timezone"] = "Gunakan zona waktu IANA, misalnya Asia/Jakarta."
	}
	if input.EventID != nil {
		if !httpapi.ValidID(*input.EventID) {
			fields["event_id"] = "Pilih acara yang valid."
		} else {
			event := strings.ToLower(*input.EventID)
			input.EventID = &event
		}
	}
	if len(fields) == 0 {
		return nil
	}
	return fields
}

type shareInput struct {
	ExpiresAt *time.Time `json:"expires_at"`
	Scope     *string    `json:"scope"`
}

// shareExpiry: default 7 hari, paling lama 30 hari dari sekarang.
func shareExpiry(input shareInput, now time.Time) (time.Time, fieldErrors) {
	if input.Scope != nil && *input.Scope != "summary" {
		return time.Time{}, fieldErrors{"scope": "Cakupan tautan yang tersedia hanya ringkasan."}
	}
	if input.ExpiresAt == nil {
		return now.Add(defaultShareTTL), nil
	}
	expires := input.ExpiresAt.UTC()
	if !expires.After(now.Add(time.Hour)) || expires.After(now.Add(maxShareTTL)) {
		return time.Time{}, fieldErrors{"expires_at": "Masa berlaku tautan antara 1 jam dan 30 hari."}
	}
	return expires, nil
}

func validID(c *gin.Context, id string) bool {
	if httpapi.ValidID(id) {
		return true
	}
	writeError(c, errNotFound)
	return false
}

func (s Store) requestReport(c *gin.Context, account auth.Account) {
	business := c.Param("id")
	if !validID(c, business) {
		return
	}
	input, ok := httpapi.ReadJSON[reportInput](c, maxInput)
	if !ok {
		return
	}
	if fields := validateReport(&input, time.Now()); fields != nil {
		writeError(c, fields)
		return
	}
	report, err := s.create(c.Request.Context(), account.ID, strings.ToLower(business), input)
	if err != nil {
		writeError(c, err)
		return
	}
	httpapi.Success(c, http.StatusAccepted, gin.H{"job_id": report.ID, "status": report.Status, "report_id": report.ID})
}

func (s Store) getJob(c *gin.Context, account auth.Account) {
	id := c.Param("id")
	if !validID(c, id) {
		return
	}
	report, err := s.get(c.Request.Context(), account.ID, strings.ToLower(id))
	if err != nil {
		writeError(c, err)
		return
	}
	httpapi.Success(c, http.StatusOK, gin.H{"job_id": report.ID, "status": report.Status, "report_id": report.ID})
}

func (s Store) listReports(c *gin.Context, account auth.Account) {
	business := c.Param("id")
	if !validID(c, business) {
		return
	}
	data, err := s.list(c.Request.Context(), account.ID, strings.ToLower(business))
	if err != nil {
		writeError(c, err)
		return
	}
	if data == nil {
		data = []Report{}
	}
	httpapi.Success(c, http.StatusOK, data)
}

func (s Store) getReport(c *gin.Context, account auth.Account) {
	id := c.Param("id")
	if !validID(c, id) {
		return
	}
	report, err := s.get(c.Request.Context(), account.ID, strings.ToLower(id))
	if err != nil {
		writeError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	httpapi.Success(c, http.StatusOK, report)
}

func (s Store) postShare(c *gin.Context, account auth.Account, origin string) {
	id := c.Param("id")
	if !validID(c, id) {
		return
	}
	var input shareInput
	if c.Request.ContentLength != 0 {
		parsed, ok := httpapi.ReadJSON[shareInput](c, maxInput)
		if !ok {
			return
		}
		input = parsed
	}
	expires, fields := shareExpiry(input, time.Now())
	if fields != nil {
		writeError(c, fields)
		return
	}
	share, err := s.createShare(c.Request.Context(), account.ID, strings.ToLower(id), expires, origin)
	if err != nil {
		writeError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	httpapi.Success(c, http.StatusCreated, share)
}

func (s Store) listShares(c *gin.Context, account auth.Account) {
	id := c.Param("id")
	if !validID(c, id) {
		return
	}
	data, err := s.shares(c.Request.Context(), account.ID, strings.ToLower(id))
	if err != nil {
		writeError(c, err)
		return
	}
	if data == nil {
		data = []Share{}
	}
	httpapi.Success(c, http.StatusOK, data)
}

func (s Store) deleteShare(c *gin.Context, account auth.Account) {
	id := c.Param("id")
	if !validID(c, id) {
		return
	}
	if err := s.revokeShare(c.Request.Context(), account.ID, strings.ToLower(id)); err != nil {
		writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (s Store) getPublic(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("X-Robots-Tag", "noindex")
	if s.DB == nil {
		httpapi.Error(c, http.StatusServiceUnavailable, "REPORTS_UNAVAILABLE", "Rapor belum dapat dibuka.", nil)
		return
	}
	token := c.Param("token")
	if len(token) < 20 || len(token) > 100 {
		writeError(c, errNotFound)
		return
	}
	report, err := s.public(c.Request.Context(), token)
	if err != nil {
		writeError(c, err)
		return
	}
	httpapi.Success(c, http.StatusOK, report)
}

func writeError(c *gin.Context, err error) {
	var fields fieldErrors
	switch {
	case errors.As(err, &fields):
		httpapi.Error(c, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Periksa isian rapor.", fields)
	case errors.Is(err, errNotFound), errors.Is(err, pgx.ErrNoRows):
		httpapi.Error(c, http.StatusNotFound, "NOT_FOUND", "Rapor tidak ditemukan atau tautan sudah tidak berlaku.", nil)
	default:
		httpapi.LogInternalError(c, "report request failed", err)
		httpapi.Error(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Rapor belum dapat diproses.", nil)
	}
}
