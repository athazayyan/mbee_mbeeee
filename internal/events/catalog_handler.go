package events

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

func catalogUnavailable(c *gin.Context) {
	httpapi.Error(c, http.StatusServiceUnavailable, "CATALOG_UNAVAILABLE", "Katalog acara belum dapat dimuat.", nil)
}

// readCatalogFilter validates the public query string. Field names follow the listEvents contract.
func readCatalogFilter(c *gin.Context) (catalogFilter, fieldErrors) {
	fields := make(fieldErrors)
	filter := catalogFilter{
		Slug:           c.Query("slug"),
		City:           strings.TrimSpace(c.Query("city")),
		CategoryID:     strings.ToLower(c.Query("category_id")),
		OrganizationID: strings.ToLower(c.Query("organization_id")),
		Query:          strings.TrimSpace(c.Query("q")),
		Status:         c.Query("status"),
	}
	limit, err := strconv.Atoi(c.DefaultQuery("limit", "20"))
	if err != nil || limit < 1 || limit > 100 {
		fields["limit"] = "Gunakan limit antara 1 dan 100."
	}
	filter.Limit = limit
	if filter.Slug != "" && (len(filter.Slug) > 120 || !slugPattern.MatchString(filter.Slug)) {
		fields["slug"] = "Gunakan slug yang valid."
	}
	if filter.City != "" && !httpapi.ValidSingleLine(filter.City, 120) {
		fields["city"] = "Gunakan nama kota satu baris hingga 120 karakter."
	}
	if filter.Query != "" && !httpapi.ValidSingleLine(filter.Query, 120) {
		fields["q"] = "Gunakan kata kunci satu baris hingga 120 karakter."
	}
	if filter.CategoryID != "" && !httpapi.ValidID(filter.CategoryID) {
		fields["category_id"] = "Pilih kategori usaha yang valid."
	}
	if filter.OrganizationID != "" && !httpapi.ValidID(filter.OrganizationID) {
		fields["organization_id"] = "Pilih penyelenggara yang valid."
	}
	switch filter.Status {
	case "", "published", "live":
	default:
		fields["status"] = "Pilih status published atau live."
	}
	for _, item := range []struct {
		name   string
		target **time.Time
	}{{"starts_from", &filter.StartsFrom}, {"ends_before", &filter.EndsBefore}} {
		raw := c.Query(item.name)
		if raw == "" {
			continue
		}
		at, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			fields[item.name] = "Gunakan tanggal dan waktu RFC 3339, misalnya 2026-12-01T00:00:00+07:00."
			continue
		}
		*item.target = &at
	}
	after, afterID, err := httpapi.DecodeCursor(c.Query("cursor"))
	if err != nil {
		fields["cursor"] = "Gunakan cursor dari halaman sebelumnya."
	}
	filter.AfterAt, filter.AfterID = after, afterID
	if len(fields) > 0 {
		return filter, fields
	}
	return filter, nil
}

func (s Store) listPublicEvents(c *gin.Context) {
	if s.DB == nil {
		catalogUnavailable(c)
		return
	}
	filter, fields := readCatalogFilter(c)
	if fields != nil {
		writeError(c, fields)
		return
	}
	limit := filter.Limit
	filter.Limit = limit + 1
	data, err := s.listPublic(c.Request.Context(), filter)
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
	if data == nil {
		data = []publicEvent{}
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{"next_cursor": next}})
}

func (s Store) getPublicEvent(c *gin.Context) {
	if s.DB == nil {
		catalogUnavailable(c)
		return
	}
	id := c.Param("id")
	if !validResource(c, id) {
		return
	}
	data, err := s.getPublic(c.Request.Context(), id)
	if err != nil {
		writeError(c, err)
		return
	}
	httpapi.Success(c, http.StatusOK, data)
}
