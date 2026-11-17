package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestRateLimitBlocksAfterLimitAndResets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(RateLimit(2, 80*time.Millisecond))
	router.GET("/", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	call := func() int {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
		return w.Code
	}
	if call() != 204 || call() != 204 {
		t.Fatal("requests within the limit must pass")
	}
	if got := call(); got != http.StatusTooManyRequests {
		t.Fatalf("third request: %d", got)
	}
	time.Sleep(100 * time.Millisecond)
	if call() != 204 {
		t.Fatal("the window must reset")
	}
}
