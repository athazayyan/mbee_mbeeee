package events

import (
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

func catalogContext(query string) *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/api/v1/events?"+query, nil)
	return c
}

func TestReadCatalogFilterDefaults(t *testing.T) {
	filter, fields := readCatalogFilter(catalogContext(""))
	if fields != nil {
		t.Fatalf("fields = %v, want nil", fields)
	}
	if filter.Limit != 20 || !slices.Equal(filter.statuses(), catalogDefaultStatuses) {
		t.Fatalf("filter = %+v, want limit 20 and published/live", filter)
	}
}

func TestReadCatalogFilterAcceptsAllFilters(t *testing.T) {
	cursor := httpapi.EncodeCursor(mustTime(t, "2026-12-01T09:00:00+07:00"), standA)
	query := "slug=bazar-akhir-tahun&city=Banda%20Aceh&q=kopi&status=live&limit=5" +
		"&category_id=0B0C0000-0000-4000-8000-000000000001&organization_id=" + standB +
		"&starts_from=2026-12-01T00:00:00%2B07:00&ends_before=2026-12-31T23:59:59%2B07:00&cursor=" + cursor
	filter, fields := readCatalogFilter(catalogContext(query))
	if fields != nil {
		t.Fatalf("fields = %v, want nil", fields)
	}
	if filter.CategoryID != blockA || filter.AfterID != standA || filter.StartsFrom == nil || filter.EndsBefore == nil ||
		filter.Limit != 5 || !slices.Equal(filter.statuses(), []string{"live"}) {
		t.Fatalf("filter = %+v", filter)
	}
}

func TestCatalogSlugIncludesFinishedEvents(t *testing.T) {
	filter, _ := readCatalogFilter(catalogContext("slug=bazar-lama"))
	if !slices.Equal(filter.statuses(), catalogPublicStatuses) {
		t.Fatalf("statuses = %v, want every public status for a slug lookup", filter.statuses())
	}
}

func TestReadCatalogFilterRejectsInvalidInput(t *testing.T) {
	cases := map[string]string{
		"limit=0":                "limit",
		"limit=101":              "limit",
		"slug=Bukan_Slug":        "slug",
		"status=draft":           "status",
		"category_id=bukan-uuid": "category_id",
		"organization_id=123":    "organization_id",
		"starts_from=2026-12-01": "starts_from",
		"ends_before=besok":      "ends_before",
		"cursor=%25%25%25":       "cursor",
		"q=%07":                  "q",
	}
	for query, field := range cases {
		t.Run(query, func(t *testing.T) {
			_, fields := readCatalogFilter(catalogContext(query))
			if fields[field] == "" {
				t.Fatalf("fields = %v, want an error for %s", fields, field)
			}
		})
	}
}

func mustTime(t *testing.T, value string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatal(err)
	}
	return at
}
