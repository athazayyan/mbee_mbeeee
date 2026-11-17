package identity

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"kitakarsa.com/tenantku/backend/internal/auth"
	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

const (
	analyticsPolicy = "analytics-v1"
	maxInputSize    = 4096
)

func (s Store) Routes(api *gin.RouterGroup, sessions *auth.Service, origin string) {
	api.GET("/me", sessions.WithAccount(s.me))
	// Kategori usaha bersifat publik: situs memakainya untuk filter katalog tanpa login.
	api.GET("/business-categories", s.listPublicCategories)
	api.GET("/businesses/:id", sessions.WithAccount(s.getBusiness))
	api.GET("/businesses/:id/consents", sessions.WithAccount(s.listConsents))
	api.GET("/organizations/:id", sessions.WithAccount(s.getOrganization))

	mutations := api.Group("")
	if sessions != nil {
		mutations.Use(httpapi.RequireOrigin(origin))
	}
	mutations.POST("/businesses", sessions.WithAccount(s.addBusiness))
	mutations.PATCH("/businesses/:id", sessions.WithAccount(s.patchBusiness))
	mutations.POST("/businesses/:id/consents", sessions.WithAccount(s.addConsent))
	mutations.POST("/organizations", sessions.WithAccount(s.addOrganization))
	mutations.PATCH("/organizations/:id", sessions.WithAccount(s.patchOrganization))
}

type accountProfile struct {
	auth.Account
	Memberships []membership `json:"memberships"`
	// Phone and PlannedEventName come from the registration form and are absent for older accounts.
	Phone            *string `json:"phone,omitempty"`
	PlannedEventName *string `json:"planned_event_name,omitempty"`
	// Roles menentukan menu yang ditampilkan app. Hak akses tetap diperiksa per endpoint.
	Roles []string `json:"roles"`
}

// profileRoles: tenant bila punya usaha, organizer bila anggota organisasi, admin bila admin platform.
func profileRoles(memberships []membership, admin bool) []string {
	roles := []string{}
	var tenant, organizer bool
	for _, item := range memberships {
		tenant = tenant || item.SubjectType == "business"
		organizer = organizer || item.SubjectType == "organization"
	}
	if tenant {
		roles = append(roles, "tenant")
	}
	if organizer {
		roles = append(roles, "organizer")
	}
	if admin {
		roles = append(roles, "admin")
	}
	return roles
}

func (s Store) me(c *gin.Context, account auth.Account) {
	profile := accountProfile{Account: account}
	memberships, err := s.memberships(c.Request.Context(), account.ID)
	if err != nil {
		writeResult(c, http.StatusOK, accountProfile{}, err)
		return
	}
	profile.Memberships = memberships
	if profile.Phone, profile.PlannedEventName, err = s.signup(c.Request.Context(), account.ID); err != nil {
		writeResult(c, http.StatusOK, accountProfile{}, err)
		return
	}
	admin, err := s.isPlatformAdmin(c.Request.Context(), account.ID)
	profile.Roles = profileRoles(memberships, admin)
	writeResult(c, http.StatusOK, profile, err)
}

func (s Store) listPublicCategories(c *gin.Context) {
	if s.DB == nil {
		httpapi.Error(c, http.StatusServiceUnavailable, "CATEGORIES_UNAVAILABLE", "Kategori usaha belum dapat dimuat.", nil)
		return
	}
	categories, err := s.categories(c.Request.Context())
	writeResult(c, http.StatusOK, categories, err)
}

type businessInput struct {
	Name       *string `json:"name"`
	CategoryID *string `json:"category_id"`
}

func validateBusiness(input businessInput, create bool) map[string]string {
	fields := make(map[string]string)
	if (create || input.Name != nil) && !validName(input.Name) {
		fields["name"] = "Masukkan nama usaha dengan panjang 1–120 karakter."
	}
	if (create && input.CategoryID == nil) || (input.CategoryID != nil && !httpapi.ValidID(*input.CategoryID)) {
		fields["category_id"] = "Pilih kategori usaha yang valid."
	}
	if !create && input.Name == nil && input.CategoryID == nil {
		fields["name"] = "Isi setidaknya satu perubahan."
	}
	return fields
}

func (s Store) addBusiness(c *gin.Context, account auth.Account) {
	input, ok := httpapi.ReadJSON[businessInput](c, maxInputSize)
	if !ok || !validate(c, validateBusiness(input, true)) {
		return
	}
	business, err := s.createBusiness(c.Request.Context(), account.ID, *input.Name, *input.CategoryID)
	writeResult(c, http.StatusCreated, business, err)
}

func (s Store) getBusiness(c *gin.Context, account auth.Account) {
	id, ok := resourceID(c)
	if !ok {
		return
	}
	business, err := s.business(c.Request.Context(), account.ID, id)
	writeResult(c, http.StatusOK, business, err)
}

func (s Store) patchBusiness(c *gin.Context, account auth.Account) {
	id, ok := resourceID(c)
	if !ok {
		return
	}
	input, ok := httpapi.ReadJSON[businessInput](c, maxInputSize)
	if !ok || !validate(c, validateBusiness(input, false)) {
		return
	}
	business, err := s.updateBusiness(c.Request.Context(), account.ID, id, input.Name, input.CategoryID)
	writeResult(c, http.StatusOK, business, err)
}

type organizationInput struct {
	Name *string `json:"name"`
}

func readOrganizationName(c *gin.Context) (string, bool) {
	input, ok := httpapi.ReadJSON[organizationInput](c, maxInputSize)
	if !ok {
		return "", false
	}
	if !validName(input.Name) {
		validate(c, map[string]string{"name": "Masukkan nama organisasi dengan panjang 1–120 karakter."})
		return "", false
	}
	return *input.Name, true
}

func (s Store) addOrganization(c *gin.Context, account auth.Account) {
	name, ok := readOrganizationName(c)
	if !ok {
		return
	}
	organization, err := s.createOrganization(c.Request.Context(), account.ID, name)
	writeResult(c, http.StatusCreated, organization, err)
}

func (s Store) getOrganization(c *gin.Context, account auth.Account) {
	id, ok := resourceID(c)
	if !ok {
		return
	}
	organization, err := s.organization(c.Request.Context(), account.ID, id)
	writeResult(c, http.StatusOK, organization, err)
}

func (s Store) patchOrganization(c *gin.Context, account auth.Account) {
	id, ok := resourceID(c)
	if !ok {
		return
	}
	name, ok := readOrganizationName(c)
	if !ok {
		return
	}
	organization, err := s.updateOrganization(c.Request.Context(), account.ID, id, name)
	writeResult(c, http.StatusOK, organization, err)
}

type consentInput struct {
	Purpose       string `json:"purpose"`
	Granted       *bool  `json:"granted"`
	PolicyVersion string `json:"policy_version"`
}

func (s Store) listConsents(c *gin.Context, account auth.Account) {
	id, ok := resourceID(c)
	if !ok {
		return
	}
	events, err := s.consents(c.Request.Context(), account.ID, id)
	writeResult(c, http.StatusOK, events, err)
}

func (s Store) addConsent(c *gin.Context, account auth.Account) {
	id, ok := resourceID(c)
	if !ok {
		return
	}
	input, ok := httpapi.ReadJSON[consentInput](c, maxInputSize)
	if !ok {
		return
	}
	fields := make(map[string]string)
	if input.Purpose != "analytics" {
		fields["purpose"] = "Tujuan persetujuan tidak tersedia."
	}
	if input.Granted == nil {
		fields["granted"] = "Tentukan status persetujuan."
	}
	if input.PolicyVersion != analyticsPolicy {
		fields["policy_version"] = "Muat ulang halaman untuk melihat versi kebijakan yang berlaku."
	}
	if !validate(c, fields) {
		return
	}
	event, err := s.recordConsent(c.Request.Context(), account.ID, id, input.Purpose, *input.Granted, input.PolicyVersion)
	writeResult(c, http.StatusCreated, event, err)
}

func validate(c *gin.Context, fields map[string]string) bool {
	if len(fields) == 0 {
		return true
	}
	httpapi.Error(c, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Periksa isian profil.", fields)
	return false
}

func validName(name *string) bool {
	if name == nil {
		return false
	}
	*name = strings.TrimSpace(*name)
	return httpapi.ValidSingleLine(*name, 120)
}

func resourceID(c *gin.Context) (string, bool) {
	id := c.Param("id")
	if !httpapi.ValidID(id) {
		httpapi.Error(c, http.StatusNotFound, "NOT_FOUND", "Profil tidak ditemukan.", nil)
		return "", false
	}
	return id, true
}

func writeResult(c *gin.Context, status int, data any, err error) {
	var dbError *pgconn.PgError
	switch {
	case err == nil:
		httpapi.Success(c, status, data)
	case errors.Is(err, pgx.ErrNoRows):
		httpapi.Error(c, http.StatusNotFound, "NOT_FOUND", "Profil tidak ditemukan.", nil)
	case errors.Is(err, errBusinessAlreadyExists):
		httpapi.Error(c, http.StatusConflict, "BUSINESS_ALREADY_EXISTS", "Akun ini sudah memiliki profil usaha.", nil)
	case errors.As(err, &dbError) && dbError.ConstraintName == "businesses_category_id_fkey":
		validate(c, map[string]string{"category_id": "Pilih kategori usaha yang tersedia."})
	default:
		httpapi.LogInternalError(c, "identity request failed", err)
		httpapi.Error(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Profil belum dapat diproses.", nil)
	}
}
