package events

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"kitakarsa.com/tenantku/backend/internal/auth"
	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

const maxEventInput = 64 << 10

var slugPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

type eventInput struct {
	Name        *string    `json:"name"`
	Slug        *string    `json:"slug"`
	StartsAt    *time.Time `json:"starts_at"`
	EndsAt      *time.Time `json:"ends_at"`
	Timezone    *string    `json:"timezone"`
	City        *string    `json:"city"`
	Address     *string    `json:"address"`
	Description *string    `json:"description"`
	Facilities  *[]string  `json:"facilities"`
	Terms       *string    `json:"terms"`
}

func (input eventInput) apply(value event) event {
	value.Name = existing(input.Name, value.Name)
	value.Slug = existing(input.Slug, value.Slug)
	value.StartsAt = existing(input.StartsAt, value.StartsAt)
	value.EndsAt = existing(input.EndsAt, value.EndsAt)
	value.Timezone = existing(input.Timezone, value.Timezone)
	value.City = existing(input.City, value.City)
	value.Address = existing(input.Address, value.Address)
	value.Facilities = existing(input.Facilities, value.Facilities)
	if input.Description != nil {
		value.Description = nilIfEmpty(*input.Description)
	}
	if input.Terms != nil {
		value.Terms = nilIfEmpty(*input.Terms)
	}
	return value
}

func existing[T any](input *T, current T) T {
	if input != nil {
		return *input
	}
	return current
}

func (s Store) Routes(api *gin.RouterGroup, sessions *auth.Service, origin string) {
	api.GET("/organizations/:id/events", sessions.WithAccount(s.listEvents))
	api.GET("/organizations/:id/events/:event_id", sessions.WithAccount(s.getEvent))
	api.GET("/organizations/:id/event-history", sessions.WithAccount(s.listHistoricalEvents))
	api.GET("/events/:id/pricing", sessions.WithAccount(s.getPricing))
	api.GET("/events/:id/revisions", sessions.WithAccount(s.listRevisions))
	api.GET("/bookings/:id/event-revisions", sessions.WithAccount(s.listBookingRevisions))
	api.GET("/events", s.listPublicEvents)
	api.GET("/events/:id", s.getPublicEvent)
	api.GET("/events/:id/layout", s.getPublicLayout)
	api.GET("/organizations/:id/events/:event_id/layout", sessions.WithAccount(s.getOrganizerLayout))
	mutations := api.Group("")
	if sessions != nil {
		mutations.Use(httpapi.RequireOrigin(origin))
	}
	mutations.POST("/organizations/:id/events", sessions.WithAccount(s.createEvent))
	mutations.POST("/organizations/:id/event-history", sessions.WithAccount(s.recordHistoricalEvent))
	mutations.PATCH("/events/:id", sessions.WithAccount(s.updateEvent))
	mutations.POST("/events/:id/duplicate", sessions.WithAccount(s.duplicateEvent))
	mutations.POST("/events/:id/cancel", sessions.WithAccount(s.cancelEvent))
	mutations.PUT("/events/:id/pricing", sessions.WithAccount(s.replacePricing))
	mutations.POST("/events/:id/pricing/preview", sessions.WithAccount(s.previewPrice))
	mutations.POST("/events/:id/revisions", sessions.WithAccount(s.proposeRevision))
	mutations.PUT("/bookings/:id/event-revisions/:revision_id/decision", sessions.WithAccount(s.decideBookingRevision))
	mutations.PUT("/events/:id/layout", sessions.WithAccount(s.replaceLayout))
}

func (s Store) listEvents(c *gin.Context, account auth.Account) {
	org := c.Param("id")
	if !validResource(c, org) {
		return
	}
	limit, err := strconv.Atoi(c.DefaultQuery("limit", "20"))
	if err != nil || limit < 1 || limit > 100 {
		writeError(c, fieldErrors{"limit": "Gunakan limit antara 1 dan 100."})
		return
	}
	before, beforeID, err := httpapi.DecodeCursor(c.Query("cursor"))
	if err != nil {
		writeError(c, fieldErrors{"cursor": "Gunakan cursor dari halaman sebelumnya."})
		return
	}
	query := strings.TrimSpace(c.Query("q"))
	if query != "" && !httpapi.ValidSingleLine(query, 120) {
		writeError(c, fieldErrors{"q": "Gunakan kata kunci satu baris hingga 120 karakter."})
		return
	}
	status := c.Query("status")
	switch status {
	case "", "draft", "published", "live", "completed", "cancelled":
	default:
		writeError(c, fieldErrors{"status": "Pilih status acara yang valid."})
		return
	}
	data, err := s.list(c.Request.Context(), account.ID, org, before, beforeID, limit+1, query, status)
	if err != nil {
		writeError(c, err)
		return
	}
	var next *string
	if len(data) > limit {
		data = data[:limit]
		last := data[len(data)-1]
		cursor := httpapi.EncodeCursor(last.StartsAt, last.ID)
		next = &cursor
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{"next_cursor": next}})
}

func (s Store) getEvent(c *gin.Context, account auth.Account) {
	org, id := c.Param("id"), c.Param("event_id")
	if !validResource(c, org) || !validResource(c, id) {
		return
	}
	data, err := s.get(c.Request.Context(), account.ID, org, id)
	writeEvent(c, http.StatusOK, data, err)
}

func (s Store) createEvent(c *gin.Context, account auth.Account) {
	org := c.Param("id")
	if !validResource(c, org) {
		return
	}
	input, ok := readEvent(c, true)
	if !ok {
		return
	}
	data, err := s.create(c.Request.Context(), account.ID, org, input)
	writeEvent(c, http.StatusCreated, data, err)
}

func (s Store) updateEvent(c *gin.Context, account auth.Account) {
	id := c.Param("id")
	if !validResource(c, id) {
		return
	}
	input, ok := readEvent(c, false)
	if !ok {
		return
	}
	data, err := s.update(c.Request.Context(), account.ID, id, input)
	writeEvent(c, http.StatusOK, data, err)
}

func readEvent(c *gin.Context, create bool) (eventInput, bool) {
	input, ok := httpapi.ReadJSON[eventInput](c, maxEventInput)
	if !ok {
		return input, false
	}
	if fields := validateEventInput(input, create, true); len(fields) > 0 {
		writeError(c, fields)
		return input, false
	}
	return input, true
}

func validateEventInput(input eventInput, create, withSlug bool) fieldErrors {
	fields := make(fieldErrors)
	for _, field := range []struct {
		name  string
		value *string
		max   int
	}{
		{"name", input.Name, 160},
		{"slug", input.Slug, 120},
		{"timezone", input.Timezone, 64},
		{"city", input.City, 120},
		{"address", input.Address, 300},
	} {
		if field.name == "slug" && !withSlug {
			continue
		}
		if field.value == nil {
			if create {
				fields[field.name] = "Isian ini wajib."
			}
		} else {
			*field.value = strings.TrimSpace(*field.value)
			if !httpapi.ValidSingleLine(*field.value, field.max) {
				fields[field.name] = "Isian satu baris tidak valid."
			}
		}
	}
	if input.Slug != nil && !slugPattern.MatchString(*input.Slug) {
		fields["slug"] = "Gunakan huruf kecil, angka, dan tanda hubung."
	}
	if input.Timezone != nil {
		_, err := time.LoadLocation(*input.Timezone)
		if err != nil || *input.Timezone == "Local" {
			fields["timezone"] = "Pilih zona waktu IANA seperti Asia/Jakarta."
		}
	}
	if (create && input.StartsAt == nil) || (input.StartsAt != nil && input.StartsAt.IsZero()) {
		fields["starts_at"] = "Tentukan waktu mulai."
	}
	if (create && input.EndsAt == nil) || (input.EndsAt != nil && input.EndsAt.IsZero()) {
		fields["ends_at"] = "Tentukan waktu selesai."
	}
	if input.StartsAt != nil && input.EndsAt != nil && !input.EndsAt.After(*input.StartsAt) {
		fields["ends_at"] = "Waktu selesai harus sesudah waktu mulai."
	}
	for _, field := range []struct {
		name  string
		value *string
		max   int
	}{{"description", input.Description, 3000}, {"terms", input.Terms, 6000}} {
		if field.value != nil {
			*field.value = strings.TrimSpace(*field.value)
			if utf8.RuneCountInString(*field.value) > field.max || strings.ContainsRune(*field.value, 0) {
				fields[field.name] = "Isi terlalu panjang atau memuat karakter yang tidak valid."
			}
		}
	}
	if input.Facilities != nil {
		if len(*input.Facilities) > 20 {
			fields["facilities"] = "Masukkan maksimal 20 fasilitas."
		} else {
			for index, item := range *input.Facilities {
				(*input.Facilities)[index] = strings.TrimSpace(item)
				if !httpapi.ValidSingleLine((*input.Facilities)[index], 120) {
					fields["facilities"] = "Masukkan nama fasilitas yang valid."
					break
				}
			}
		}
	}
	if !create && input == (eventInput{}) {
		fields["name"] = "Isi setidaknya satu perubahan."
	}
	return fields
}

func validResource(c *gin.Context, id string) bool {
	if httpapi.ValidID(id) {
		return true
	}
	writeError(c, pgx.ErrNoRows)
	return false
}

type fieldErrors map[string]string

func (fieldErrors) Error() string { return "event validation failed" }

func writeEvent(c *gin.Context, status int, data event, err error) {
	if err != nil {
		writeError(c, err)
		return
	}
	httpapi.Success(c, status, data)
}

func writeError(c *gin.Context, err error) {
	if writeLayoutError(c, err) {
		return
	}
	var fields fieldErrors
	var dbError *pgconn.PgError
	switch {
	case errors.As(err, &fields):
		httpapi.Error(c, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Periksa isian acara.", fields)
	case errors.Is(err, pgx.ErrNoRows):
		httpapi.Error(c, http.StatusNotFound, "NOT_FOUND", "Acara atau organisasi tidak ditemukan.", nil)
	case errors.Is(err, errDraftRequired):
		httpapi.Error(c, http.StatusConflict, "EVENT_NOT_DRAFT", "Aksi ini hanya tersedia pada draft acara.", nil)
	case errors.Is(err, errPricingClosed):
		httpapi.Error(c, http.StatusConflict, "EVENT_PRICING_CLOSED", "Harga acara sudah tidak dapat diubah.", nil)
	case errors.Is(err, errRevisionRequired):
		httpapi.Error(c, http.StatusConflict, "EVENT_REVISION_REQUIRED", "Perubahan material perlu diajukan sebagai revisi karena acara memiliki booking aktif.", nil)
	case errors.Is(err, errRevisionUnavailable):
		httpapi.Error(c, http.StatusConflict, "EVENT_REVISION_UNAVAILABLE", "Revisi tersedia untuk acara terbit atau berlangsung yang memiliki booking aktif.", nil)
	case errors.Is(err, errIdempotencyKeyMismatch):
		httpapi.Error(c, http.StatusConflict, "IDEMPOTENCY_PAYLOAD_MISMATCH", "Gunakan key baru untuk permintaan yang berbeda.", nil)
	case errors.As(err, &dbError) && dbError.ConstraintName == "events_slug_key":
		httpapi.Error(c, http.StatusConflict, "SLUG_IN_USE", "Slug sudah digunakan.", map[string]string{"slug": "Gunakan slug lain."})
	default:
		httpapi.LogInternalError(c, "event request failed", err)
		httpapi.Error(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Acara belum dapat diproses.", nil)
	}
}
