package httpapi

import (
	"crypto/rand"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
)

const requestIDKey = "request_id"

func RequireOrigin(origin string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.GetHeader("Origin") != origin {
			Error(c, http.StatusForbidden, "ORIGIN_REJECTED", "Origin tidak diizinkan.", nil)
		}
	}
}

func RequestID(c *gin.Context) {
	var id [16]byte
	rand.Read(id[:])
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	requestID := fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:])
	c.Set(requestIDKey, requestID)
	c.Header("X-Request-ID", requestID)
	c.Next()
}

func Success(c *gin.Context, status int, data any) {
	c.JSON(status, gin.H{"data": data, "meta": gin.H{}})
}

func Error(c *gin.Context, status int, code, message string, fields map[string]string) {
	if fields == nil {
		fields = map[string]string{}
	}
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{
		"code": code, "message": message, "fields": fields, "request_id": c.GetString(requestIDKey),
	}})
}
