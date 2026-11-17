package admin

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

const maxInput = 4 << 10

func (s Store) Routes(api *gin.RouterGroup, sessions *auth.Service, origin string) {
	api.GET("/plans/catalog", s.listPlans)
	api.GET("/plans/quote", s.quote)
	api.GET("/admin/overview", sessions.WithAccount(s.withAdmin(s.getOverview)))
	api.GET("/admin/organizations", sessions.WithAccount(s.withAdmin(s.listOrganizations)))
	api.GET("/admin/users", sessions.WithAccount(s.withAdmin(s.listUsers)))
	api.GET("/admin/audit", sessions.WithAccount(s.withAdmin(s.listAudit)))
	mutations := api.Group("")
	if sessions != nil {
		mutations.Use(httpapi.RequireOrigin(origin))
	}
	mutations.POST("/admin/organizations/:id/verification", sessions.WithAccount(s.verifyOrganization))
}

// withAdmin menolak akun yang bukan admin platform. Mutasi memeriksa ulang di dalam transaksinya.
func (s Store) withAdmin(next func(*gin.Context, auth.Account)) func(*gin.Context, auth.Account) {
	return func(c *gin.Context, account auth.Account) {
		admin, err := s.IsAdmin(c.Request.Context(), account.ID)
		if err != nil {
			writeError(c, err)
			return
		}
		if !admin {
			writeError(c, errForbidden)
			return
		}
		next(c, account)
	}
}

func (s Store) listPlans(c *gin.Context) {
	if s.DB == nil {
		httpapi.Error(c, http.StatusServiceUnavailable, "PLANS_UNAVAILABLE", "Daftar paket belum dapat dimuat.", nil)
		return
	}
	catalog, err := s.planCatalog(c.Request.Context())
	if err != nil {
		writeError(c, err)
		return
	}
	httpapi.Success(c, http.StatusOK, catalog)
}

func (s Store) quote(c *gin.Context) {
	if s.DB == nil {
		httpapi.Error(c, http.StatusServiceUnavailable, "PLANS_UNAVAILABLE", "Daftar paket belum dapat dimuat.", nil)
		return
	}
	capacity, err := strconv.ParseInt(c.Query("capacity"), 10, 64)
	if err != nil || capacity < 1 || capacity > 100_000 {
		httpapi.Error(c, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Periksa isian.",
			map[string]string{"capacity": "Isi kapasitas tenant 1 sampai 100.000."})
		return
	}
	catalog, err := s.planCatalog(c.Request.Context())
	if err != nil {
		writeError(c, err)
		return
	}
	result, ok := quotePlan(catalog, capacity)
	if !ok {
		writeError(c, pgx.ErrNoRows)
		return
	}
	httpapi.Success(c, http.StatusOK, result)
}

func (s Store) getOverview(c *gin.Context, _ auth.Account) {
	overview, err := s.overview(c.Request.Context())
	if err != nil {
		writeError(c, err)
		return
	}
	httpapi.Success(c, http.StatusOK, overview)
}

// readPage membaca q, limit, dan cursor yang sama untuk semua daftar admin.
func readPage(c *gin.Context) (pageFilter, map[string]string) {
	fields := make(map[string]string)
	limit, err := strconv.Atoi(c.DefaultQuery("limit", "50"))
	if err != nil || limit < 1 || limit > 100 {
		fields["limit"] = "Gunakan limit antara 1 dan 100."
	}
	query := strings.TrimSpace(c.Query("q"))
	if query != "" && !httpapi.ValidSingleLine(query, 120) {
		fields["q"] = "Gunakan kata kunci satu baris hingga 120 karakter."
	}
	after, afterID, err := httpapi.DecodeCursor(c.Query("cursor"))
	if err != nil {
		fields["cursor"] = "Gunakan cursor dari halaman sebelumnya."
	}
	return pageFilter{Query: query, AfterAt: after, AfterID: afterID, Limit: limit + 1}, fields
}

func page[T any](c *gin.Context, data []T, limit int, cursor func(T) string) {
	var next *string
	if len(data) > limit {
		data = data[:limit]
		value := cursor(data[len(data)-1])
		next = &value
	}
	if data == nil {
		data = []T{}
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{"next_cursor": next}})
}

func (s Store) listOrganizations(c *gin.Context, _ auth.Account) {
	filter, fields := readPage(c)
	switch status := c.Query("verification"); status {
	case "", "verified", "unverified":
		filter.Status = status
	default:
		fields["verification"] = "Pilih verified atau unverified."
	}
	if len(fields) > 0 {
		httpapi.Error(c, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Periksa filter.", fields)
		return
	}
	data, err := s.organizations(c.Request.Context(), filter)
	if err != nil {
		writeError(c, err)
		return
	}
	page(c, data, filter.Limit-1, func(o Organization) string { return httpapi.EncodeCursor(o.CreatedAt, o.ID) })
}

func (s Store) listUsers(c *gin.Context, _ auth.Account) {
	filter, fields := readPage(c)
	if len(fields) > 0 {
		httpapi.Error(c, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Periksa filter.", fields)
		return
	}
	data, err := s.users(c.Request.Context(), filter)
	if err != nil {
		writeError(c, err)
		return
	}
	page(c, data, filter.Limit-1, func(u User) string { return httpapi.EncodeCursor(u.CreatedAt, u.ID) })
}

func (s Store) listAudit(c *gin.Context, _ auth.Account) {
	filter, fields := readPage(c)
	action := strings.TrimSpace(c.Query("action"))
	if action != "" && !httpapi.ValidSingleLine(action, 80) {
		fields["action"] = "Gunakan nama aksi yang valid."
	}
	filter.Status = action
	if len(fields) > 0 {
		httpapi.Error(c, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Periksa filter.", fields)
		return
	}
	data, err := s.audit(c.Request.Context(), filter)
	if err != nil {
		writeError(c, err)
		return
	}
	page(c, data, filter.Limit-1, func(a AuditEvent) string { return httpapi.EncodeCursor(a.OccurredAt, a.ID) })
}

type verificationInput struct {
	Status string `json:"status"`
	Reason string `json:"reason"`
}

func validateVerification(input *verificationInput) map[string]string {
	fields := make(map[string]string)
	if input.Status != "verified" && input.Status != "unverified" {
		fields["status"] = "Pilih verified atau unverified."
	}
	input.Reason = strings.TrimSpace(input.Reason)
	if input.Reason == "" || len([]rune(input.Reason)) > 500 || strings.ContainsAny(input.Reason, "\x00") {
		fields["reason"] = "Tulis alasan 1 sampai 500 karakter untuk jejak audit."
	}
	if len(fields) == 0 {
		return nil
	}
	return fields
}

func (s Store) verifyOrganization(c *gin.Context, account auth.Account) {
	id := c.Param("id")
	if !httpapi.ValidID(id) {
		writeError(c, pgx.ErrNoRows)
		return
	}
	input, ok := httpapi.ReadJSON[verificationInput](c, maxInput)
	if !ok {
		return
	}
	if fields := validateVerification(&input); fields != nil {
		httpapi.Error(c, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Periksa isian verifikasi.", fields)
		return
	}
	organization, err := s.setVerification(c.Request.Context(), account.ID, strings.ToLower(id), input.Status, input.Reason,
		httpapi.RequestIDOf(c))
	if err != nil {
		writeError(c, err)
		return
	}
	httpapi.Success(c, http.StatusOK, organization)
}

func writeError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, errForbidden):
		httpapi.Error(c, http.StatusForbidden, "ADMIN_REQUIRED", "Halaman ini khusus admin platform.", nil)
	case errors.Is(err, pgx.ErrNoRows):
		httpapi.Error(c, http.StatusNotFound, "NOT_FOUND", "Data tidak ditemukan.", nil)
	default:
		httpapi.LogInternalError(c, "admin request failed", err)
		httpapi.Error(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Permintaan admin belum dapat diproses.", nil)
	}
}
