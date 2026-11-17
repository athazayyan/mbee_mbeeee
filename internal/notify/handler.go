package notify

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"kitakarsa.com/tenantku/backend/internal/auth"
	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

const (
	defaultLimit = 20
	maxLimit     = 50
)

// Routes memasang inbox notifikasi akun yang sedang masuk.
func (s Store) Routes(api *gin.RouterGroup, sessions *auth.Service, origin string) {
	api.GET("/notifications", sessions.WithAccount(s.listHandler))
	api.GET("/notifications/unread-count", sessions.WithAccount(s.unreadHandler))
	mutations := api.Group("")
	if sessions != nil {
		mutations.Use(httpapi.RequireOrigin(origin))
	}
	mutations.POST("/notifications/read-all", sessions.WithAccount(s.readAllHandler))
	mutations.POST("/notifications/:id/read", sessions.WithAccount(s.readHandler))
}

func (s Store) listHandler(c *gin.Context, account auth.Account) {
	limit := defaultLimit
	if raw := c.Query("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > maxLimit {
			validation(c, "limit", "Gunakan batas 1 sampai 50.")
			return
		}
		limit = value
	}
	before, beforeID, err := httpapi.DecodeCursor(c.Query("cursor"))
	if err != nil || (beforeID != "" && !httpapi.ValidID(beforeID)) {
		validation(c, "cursor", "Gunakan cursor dari halaman sebelumnya.")
		return
	}
	unread := c.Query("unread") == "true"
	data, unreadCount, err := s.list(c.Request.Context(), account.ID, before, beforeID, limit+1, unread)
	if err != nil {
		writeError(c, err)
		return
	}
	var next *string
	if len(data) > limit {
		data = data[:limit]
		last := data[len(data)-1]
		cursor := httpapi.EncodeCursor(last.CreatedAt, last.ID)
		next = &cursor
	}
	if data == nil {
		data = []Notification{}
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{"next_cursor": next, "unread_count": unreadCount}})
}

func (s Store) unreadHandler(c *gin.Context, account auth.Account) {
	count, err := s.unreadCount(c.Request.Context(), account.ID)
	if err != nil {
		writeError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	httpapi.Success(c, http.StatusOK, gin.H{"unread_count": count})
}

func (s Store) readHandler(c *gin.Context, account auth.Account) {
	id := c.Param("id")
	if !httpapi.ValidID(id) {
		writeError(c, errNotFound)
		return
	}
	if err := s.markRead(c.Request.Context(), account.ID, strings.ToLower(id)); err != nil {
		writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (s Store) readAllHandler(c *gin.Context, account auth.Account) {
	updated, err := s.markAllRead(c.Request.Context(), account.ID)
	if err != nil {
		writeError(c, err)
		return
	}
	httpapi.Success(c, http.StatusOK, gin.H{"updated": updated})
}

func validation(c *gin.Context, field, message string) {
	httpapi.Error(c, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Periksa permintaan notifikasi.", map[string]string{field: message})
}

func writeError(c *gin.Context, err error) {
	if errors.Is(err, errNotFound) {
		httpapi.Error(c, http.StatusNotFound, "NOT_FOUND", "Notifikasi tidak ditemukan.", nil)
		return
	}
	httpapi.LogInternalError(c, "notification request failed", err)
	httpapi.Error(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Notifikasi belum dapat dimuat.", nil)
}
