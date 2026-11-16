package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestRequestLogsCorrelateWithoutSensitiveValues(t *testing.T) {
	var output bytes.Buffer
	prior := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(prior) })

	engine := gin.New()
	engine.Use(RequestID, LogRequests, gin.CustomRecoveryWithWriter(io.Discard, RecoverRequest))
	engine.GET("/private/:id", func(c *gin.Context) {
		LogInternalError(c, "database read failed", &pgconn.PgError{Code: "42P01", Detail: "private-error-detail"})
		panic("private-panic-value")
	})
	request := httptest.NewRequest(http.MethodGet, "/private/private-id?token=private-query", nil)
	request.Header.Set("Cookie", "session=private-cookie")
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)

	var body struct {
		Error struct {
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	id := recorder.Header().Get("X-Request-ID")
	if recorder.Code != http.StatusInternalServerError || !ValidID(id) || body.Error.RequestID != id {
		t.Fatalf("unexpected response code %d or request ID %q", recorder.Code, id)
	}
	for _, field := range []string{"private-query", "private-id", "private-cookie", "private-panic-value", "private-error-detail"} {
		if strings.Contains(output.String(), field) {
			t.Fatalf("a request value appeared in request logs: %q", field)
		}
	}
	for _, expected := range []string{`"request_id":"` + id + `"`, `"route":"/private/:id"`, `"status":500`, `"error_code":"42P01"`} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("missing log field: %s", expected)
		}
	}
}
