package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"
)

func LogRequests(c *gin.Context) {
	started := time.Now()
	c.Next()
	logger := requestLogger(c)
	status := c.Writer.Status()
	level := slog.LevelInfo
	switch {
	case status >= 500:
		level = slog.LevelError
	case status >= 400:
		level = slog.LevelWarn
	}
	logger.Log(c.Request.Context(), level, "http request",
		"method", c.Request.Method,
		"status", status,
		"duration_ms", time.Since(started).Milliseconds(),
		"response_bytes", max(0, c.Writer.Size()),
	)
}

func LogInternalError(c *gin.Context, message string, err error) {
	var dbError *pgconn.PgError
	code := fmt.Sprintf("%T", err)
	switch {
	case errors.As(err, &dbError):
		code = dbError.Code
	case errors.Is(err, context.DeadlineExceeded):
		code = "deadline_exceeded"
	case errors.Is(err, context.Canceled):
		code = "cancelled"
	}
	requestLogger(c).ErrorContext(c.Request.Context(), message, "error_code", code)
}

func RecoverRequest(c *gin.Context, recovered any) {
	requestLogger(c).ErrorContext(c.Request.Context(), "http handler panic", "panic_type", fmt.Sprintf("%T", recovered))
	if c.Writer.Written() {
		c.Abort()
		return
	}
	Error(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Permintaan belum dapat diproses.", nil)
}

func requestLogger(c *gin.Context) *slog.Logger {
	path := c.FullPath()
	if path == "" {
		path = "unmatched"
	}
	return slog.With("request_id", c.GetString(requestIDKey), "route", path)
}
