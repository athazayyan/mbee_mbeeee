package reviews

import (
	"errors"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"kitakarsa.com/tenantku/backend/internal/auth"
	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

const (
	maxInput   = 8 << 10
	maxComment = 2000
	maxReason  = 500
)

type fieldErrors map[string]string

func (fieldErrors) Error() string { return "review validation failed" }

var eligibilityMessages = map[string]string{
	"event_cancelled":       "Acara dibatalkan, jadi booking ini tidak bisa diulas.",
	"booking_not_confirmed": "Hanya booking yang sudah dibayar dan terkonfirmasi yang bisa diulas.",
	"event_not_finished":    "Ulasan bisa ditulis setelah acara berakhir.",
}

// Routes memasang ulasan booking, ulasan publik penyelenggara, dan moderasi admin.
func (s Store) Routes(api *gin.RouterGroup, sessions *auth.Service, origin string) {
	api.GET("/bookings/:id/reviews", sessions.WithAccount(s.bookingHandler))
	api.GET("/organizations/:id/reviews", s.publicHandler)
	api.GET("/admin/reviews", sessions.WithAccount(s.adminListHandler))
	mutations := api.Group("")
	if sessions != nil {
		mutations.Use(httpapi.RequireOrigin(origin))
	}
	mutations.POST("/bookings/:id/reviews", sessions.WithAccount(s.createHandler))
	mutations.PATCH("/reviews/:id", sessions.WithAccount(s.updateHandler))
	mutations.POST("/admin/reviews/:id/moderation", sessions.WithAccount(s.moderateHandler))
}

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

// normalizeReview memangkas komentar; komentar kosong disimpan sebagai tidak ada.
func normalizeReview(input *reviewInput) fieldErrors {
	fields := make(fieldErrors)
	if input.Rating < 1 || input.Rating > 5 {
		fields["rating"] = "Pilih nilai 1 sampai 5 bintang."
	}
	if input.Comment != nil {
		comment := strings.TrimSpace(*input.Comment)
		switch {
		case comment == "":
			input.Comment = nil
		case !validText(comment, maxComment):
			fields["comment"] = "Komentar paling banyak 2.000 karakter."
		default:
			input.Comment = &comment
		}
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

func (s Store) bookingHandler(c *gin.Context, account auth.Account) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	data, err := s.forBooking(c.Request.Context(), account.ID, id)
	if err != nil {
		writeError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	httpapi.Success(c, http.StatusOK, data)
}

func (s Store) createHandler(c *gin.Context, account auth.Account) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	input, ok := httpapi.ReadJSON[reviewInput](c, maxInput)
	if !ok {
		return
	}
	if fields := normalizeReview(&input); fields != nil {
		writeError(c, fields)
		return
	}
	reviewID, err := s.create(c.Request.Context(), account.ID, id, input)
	if err != nil {
		writeError(c, err)
		return
	}
	httpapi.Success(c, http.StatusCreated, gin.H{"id": reviewID, "status": "published"})
}

func (s Store) updateHandler(c *gin.Context, account auth.Account) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	input, ok := httpapi.ReadJSON[reviewInput](c, maxInput)
	if !ok {
		return
	}
	if fields := normalizeReview(&input); fields != nil {
		writeError(c, fields)
		return
	}
	review, err := s.update(c.Request.Context(), account.ID, id, input)
	if err != nil {
		writeError(c, err)
		return
	}
	httpapi.Success(c, http.StatusOK, review)
}

func (s Store) publicHandler(c *gin.Context) {
	if s.DB == nil {
		httpapi.Error(c, http.StatusServiceUnavailable, "REVIEWS_UNAVAILABLE", "Ulasan belum dapat dimuat.", nil)
		return
	}
	id, ok := pathID(c)
	if !ok {
		return
	}
	data, err := s.organizer(c.Request.Context(), id)
	if err != nil {
		writeError(c, err)
		return
	}
	c.Header("Cache-Control", "public, max-age=60")
	httpapi.Success(c, http.StatusOK, data)
}

func (s Store) adminListHandler(c *gin.Context, account auth.Account) {
	status := c.DefaultQuery("status", "published")
	if status != "published" && status != "hidden" {
		writeError(c, fieldErrors{"status": "Pilih status ulasan yang valid."})
		return
	}
	data, err := s.adminList(c.Request.Context(), account.ID, status)
	if err != nil {
		writeError(c, err)
		return
	}
	if data == nil {
		data = []AdminReview{}
	}
	c.Header("Cache-Control", "no-store")
	httpapi.Success(c, http.StatusOK, data)
}

type moderationInput struct {
	Action string `json:"action"`
	Reason string `json:"reason"`
}

func validateModeration(input moderationInput) fieldErrors {
	fields := make(fieldErrors)
	if input.Action != "hide" && input.Action != "restore" {
		fields["action"] = "Pilih sembunyikan atau pulihkan ulasan."
	}
	if !validText(input.Reason, maxReason) {
		fields["reason"] = "Tulis alasan keputusan 1 sampai 500 karakter."
	}
	if len(fields) == 0 {
		return nil
	}
	return fields
}

func (s Store) moderateHandler(c *gin.Context, account auth.Account) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	input, ok := httpapi.ReadJSON[moderationInput](c, maxInput)
	if !ok {
		return
	}
	input.Reason = strings.TrimSpace(input.Reason)
	if fields := validateModeration(input); fields != nil {
		writeError(c, fields)
		return
	}
	review, err := s.moderate(c.Request.Context(), account.ID, id, input.Action, input.Reason, httpapi.RequestIDOf(c))
	if err != nil {
		writeError(c, err)
		return
	}
	httpapi.Success(c, http.StatusOK, review)
}

func writeError(c *gin.Context, err error) {
	var fields fieldErrors
	var ineligible notEligible
	switch {
	case errors.As(err, &fields):
		httpapi.Error(c, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Periksa isian ulasan.", fields)
	case errors.As(err, &ineligible):
		message := eligibilityMessages[ineligible.reason]
		httpapi.Error(c, http.StatusConflict, "REVIEW_NOT_ELIGIBLE", message, map[string]string{"reason": ineligible.reason})
	case errors.Is(err, errNotFound):
		httpapi.Error(c, http.StatusNotFound, "NOT_FOUND", "Booking atau ulasan tidak ditemukan.", nil)
	case errors.Is(err, errForbidden):
		httpapi.Error(c, http.StatusForbidden, "FORBIDDEN", "Hanya admin platform yang dapat memoderasi ulasan.", nil)
	case errors.Is(err, errExists):
		httpapi.Error(c, http.StatusConflict, "REVIEW_EXISTS", "Anda sudah memberi ulasan untuk booking ini. Ubah ulasan yang ada.", nil)
	case errors.Is(err, errLocked):
		httpapi.Error(c, http.StatusConflict, "REVIEW_LOCKED", "Ulasan ini tidak bisa diubah lagi.", nil)
	case errors.Is(err, errResolved):
		httpapi.Error(c, http.StatusConflict, "REVIEW_UNCHANGED", "Ulasan sudah berada pada status tersebut.", nil)
	default:
		httpapi.LogInternalError(c, "review request failed", err)
		httpapi.Error(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Ulasan belum dapat diproses.", nil)
	}
}
