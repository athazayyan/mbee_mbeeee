package messaging

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"kitakarsa.com/tenantku/backend/internal/auth"
	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

const (
	maxInput        = 16 << 10
	maxText         = 4000
	maxNote         = 500
	maxReason       = 500
	defaultPageSize = 30
	maxPageSize     = 100
)

type fieldErrors map[string]string

func (fieldErrors) Error() string { return "message validation failed" }

// Routes memasang chat, pelaporan pesan, dan antrean moderasi admin.
func (s Store) Routes(api *gin.RouterGroup, sessions *auth.Service, origin string) {
	api.GET("/conversations", sessions.WithAccount(s.listHandler))
	api.GET("/conversations/unread-count", sessions.WithAccount(s.unreadHandler))
	api.GET("/conversations/:id", sessions.WithAccount(s.getHandler))
	api.GET("/conversations/:id/messages", sessions.WithAccount(s.messagesHandler))
	api.GET("/admin/message-reports", sessions.WithAccount(s.reportsHandler))
	mutations := api.Group("")
	if sessions != nil {
		mutations.Use(httpapi.RequireOrigin(origin))
	}
	mutations.POST("/bookings/:id/conversation", sessions.WithAccount(s.openHandler))
	mutations.POST("/conversations/:id/messages", sessions.WithAccount(s.sendHandler))
	mutations.POST("/conversations/:id/read", sessions.WithAccount(s.readHandler))
	mutations.POST("/messages/:id/reports", sessions.WithAccount(s.reportHandler))
	mutations.POST("/admin/message-reports/:id/resolve", sessions.WithAccount(s.resolveHandler))
}

type messageInput struct {
	ClientMessageID string `json:"client_message_id"`
	Text            string `json:"text"`
}

type reportInput struct {
	Reason string  `json:"reason"`
	Note   *string `json:"note"`
}

type resolveInput struct {
	Action string `json:"action"`
	Reason string `json:"reason"`
}

// validText menerima baris baru dan tab, menolak karakter kontrol lain dan teks kosong.
func validText(text string, limit int) bool {
	if strings.TrimSpace(text) == "" || !utf8.ValidString(text) || utf8.RuneCountInString(text) > limit {
		return false
	}
	for _, r := range text {
		if unicode.IsControl(r) && r != '\n' && r != '\t' && r != '\r' {
			return false
		}
	}
	return true
}

func validateMessage(input messageInput) fieldErrors {
	fields := make(fieldErrors)
	if !httpapi.ValidID(input.ClientMessageID) {
		fields["client_message_id"] = "Gunakan UUID dari perangkat pengirim."
	}
	if !validText(input.Text, maxText) {
		fields["text"] = "Tulis pesan 1 sampai 4.000 karakter."
	}
	if len(fields) == 0 {
		return nil
	}
	return fields
}

func validateReport(input reportInput) fieldErrors {
	fields := make(fieldErrors)
	switch input.Reason {
	case "spam", "abuse", "fraud", "other":
	default:
		fields["reason"] = "Pilih alasan laporan."
	}
	if input.Note != nil {
		note := strings.TrimSpace(*input.Note)
		switch {
		case note == "":
			input.Note = nil
		case !validText(note, maxNote):
			fields["note"] = "Catatan paling banyak 500 karakter."
		}
	}
	if input.Reason == "other" && input.Note == nil {
		fields["note"] = "Jelaskan alasan laporan."
	}
	if len(fields) == 0 {
		return nil
	}
	return fields
}

func validateResolve(input resolveInput) fieldErrors {
	fields := make(fieldErrors)
	if input.Action != "hide" && input.Action != "dismiss" {
		fields["action"] = "Pilih sembunyikan pesan atau tolak laporan."
	}
	if !validText(input.Reason, maxReason) {
		fields["reason"] = "Tulis alasan keputusan 1 sampai 500 karakter."
	}
	if len(fields) == 0 {
		return nil
	}
	return fields
}

func pathID(c *gin.Context) (string, bool) {
	id := c.Param("id")
	if !httpapi.ValidID(id) {
		writeError(c, errNotFound)
		return "", false
	}
	return strings.ToLower(id), true
}

func (s Store) listHandler(c *gin.Context, account auth.Account) {
	data, err := s.list(c.Request.Context(), account.ID)
	if err != nil {
		writeError(c, err)
		return
	}
	if data == nil {
		data = []Conversation{}
	}
	c.Header("Cache-Control", "no-store")
	httpapi.Success(c, http.StatusOK, data)
}

func (s Store) unreadHandler(c *gin.Context, account auth.Account) {
	count, err := s.unreadTotal(c.Request.Context(), account.ID)
	if err != nil {
		writeError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	httpapi.Success(c, http.StatusOK, gin.H{"unread_count": count})
}

func (s Store) getHandler(c *gin.Context, account auth.Account) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	value, err := s.get(c.Request.Context(), account.ID, id)
	if err != nil {
		writeError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	httpapi.Success(c, http.StatusOK, value)
}

func (s Store) openHandler(c *gin.Context, account auth.Account) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	value, created, err := s.openForBooking(c.Request.Context(), account.ID, id)
	if err != nil {
		writeError(c, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	httpapi.Success(c, status, value)
}

func (s Store) messagesHandler(c *gin.Context, account auth.Account) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	limit := defaultPageSize
	if raw := c.Query("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > maxPageSize {
			writeError(c, fieldErrors{"limit": "Gunakan batas 1 sampai 100."})
			return
		}
		limit = value
	}
	before, beforeID, err := httpapi.DecodeCursor(c.Query("cursor"))
	if err != nil || (beforeID != "" && !httpapi.ValidID(beforeID)) {
		writeError(c, fieldErrors{"cursor": "Gunakan cursor dari halaman sebelumnya."})
		return
	}
	data, err := s.messages(c.Request.Context(), account.ID, id, before, beforeID, limit+1)
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
		data = []Message{}
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{"next_cursor": next}})
}

func (s Store) sendHandler(c *gin.Context, account auth.Account) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	input, ok := httpapi.ReadJSON[messageInput](c, maxInput)
	if !ok {
		return
	}
	if fields := validateMessage(input); fields != nil {
		writeError(c, fields)
		return
	}
	message, created, err := s.send(c.Request.Context(), account.ID, id, strings.ToLower(input.ClientMessageID), input.Text)
	if err != nil {
		writeError(c, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	httpapi.Success(c, status, message)
}

func (s Store) readHandler(c *gin.Context, account auth.Account) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	if err := s.markRead(c.Request.Context(), account.ID, id); err != nil {
		writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (s Store) reportHandler(c *gin.Context, account auth.Account) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	input, ok := httpapi.ReadJSON[reportInput](c, maxInput)
	if !ok {
		return
	}
	if fields := validateReport(input); fields != nil {
		writeError(c, fields)
		return
	}
	var note *string
	if input.Note != nil {
		if trimmed := strings.TrimSpace(*input.Note); trimmed != "" {
			note = &trimmed
		}
	}
	report, created, err := s.report(c.Request.Context(), account.ID, id, input.Reason, note)
	if err != nil {
		writeError(c, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	httpapi.Success(c, status, report)
}

func (s Store) reportsHandler(c *gin.Context, account auth.Account) {
	status := c.DefaultQuery("status", "open")
	if status != "open" && status != "actioned" && status != "dismissed" {
		writeError(c, fieldErrors{"status": "Pilih status laporan yang valid."})
		return
	}
	data, err := s.reports(c.Request.Context(), account.ID, status)
	if err != nil {
		writeError(c, err)
		return
	}
	if data == nil {
		data = []ModerationReport{}
	}
	c.Header("Cache-Control", "no-store")
	httpapi.Success(c, http.StatusOK, data)
}

func (s Store) resolveHandler(c *gin.Context, account auth.Account) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	input, ok := httpapi.ReadJSON[resolveInput](c, maxInput)
	if !ok {
		return
	}
	input.Reason = strings.TrimSpace(input.Reason)
	if fields := validateResolve(input); fields != nil {
		writeError(c, fields)
		return
	}
	report, err := s.resolve(c.Request.Context(), account.ID, id, input.Action, input.Reason, httpapi.RequestIDOf(c))
	if err != nil {
		writeError(c, err)
		return
	}
	httpapi.Success(c, http.StatusOK, report)
}

func writeError(c *gin.Context, err error) {
	var fields fieldErrors
	switch {
	case errors.As(err, &fields):
		httpapi.Error(c, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Periksa isian pesan.", fields)
	case errors.Is(err, errNotFound):
		httpapi.Error(c, http.StatusNotFound, "NOT_FOUND", "Percakapan atau pesan tidak ditemukan.", nil)
	case errors.Is(err, errForbidden):
		httpapi.Error(c, http.StatusForbidden, "FORBIDDEN", "Hanya admin platform yang dapat membuka antrean moderasi.", nil)
	case errors.Is(err, errRateLimited):
		c.Header("Retry-After", "60")
		httpapi.Error(c, http.StatusTooManyRequests, "RATE_LIMITED", "Terlalu banyak pesan dalam satu menit. Coba lagi sebentar lagi.", nil)
	case errors.Is(err, errIdempotencyMismatch):
		httpapi.Error(c, http.StatusConflict, "IDEMPOTENCY_MISMATCH", "ID pesan sudah dipakai untuk teks lain.", nil)
	case errors.Is(err, errOwnMessage):
		httpapi.Error(c, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Pesan sendiri tidak bisa dilaporkan.",
			map[string]string{"message_id": "Pesan sendiri tidak bisa dilaporkan."})
	case errors.Is(err, errResolved):
		httpapi.Error(c, http.StatusConflict, "REPORT_RESOLVED", "Laporan ini sudah ditindaklanjuti.", nil)
	default:
		httpapi.LogInternalError(c, "messaging request failed", err)
		httpapi.Error(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Pesan belum dapat diproses.", nil)
	}
}
