package httpapi

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// RateLimit allows limit requests per window for each client address. It keeps its counters in memory, so the
// limit is per server process; it is meant to keep public calculators from being hammered, not to stop abuse.
func RateLimit(limit int, window time.Duration) gin.HandlerFunc {
	type bucket struct {
		count int
		reset time.Time
	}
	var mu sync.Mutex
	buckets := make(map[string]*bucket)
	return func(c *gin.Context) {
		now := time.Now()
		key := c.ClientIP()
		mu.Lock()
		if len(buckets) > 10_000 {
			for k, b := range buckets {
				if now.After(b.reset) {
					delete(buckets, k)
				}
			}
		}
		b := buckets[key]
		if b == nil || now.After(b.reset) {
			b = &bucket{reset: now.Add(window)}
			buckets[key] = b
		}
		b.count++
		over, wait := b.count > limit, b.reset.Sub(now)
		mu.Unlock()
		if over {
			c.Header("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
			Error(c, http.StatusTooManyRequests, "RATE_LIMITED", "Terlalu banyak percobaan. Coba lagi sebentar lagi.", nil)
			c.Abort()
			return
		}
		c.Next()
	}
}
