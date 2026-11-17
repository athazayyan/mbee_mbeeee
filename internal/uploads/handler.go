package uploads

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"kitakarsa.com/tenantku/backend/internal/auth"
	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

const maxInput = 2 << 10

type fieldErrors map[string]string

func (fieldErrors) Error() string { return "upload validation failed" }

// Routes memasang alur unggah: presign → PUT isi → complete, serta pembacaan file.
func (s Store) Routes(api *gin.RouterGroup, sessions *auth.Service, origin string) {
	api.GET("/uploads/:id/file", sessions.WithAccount(s.fileHandler))
	api.GET("/public/uploads/:id", s.publicHandler)
	// Isi file dikirim ke URL bertanda tangan, bukan cookie, sehingga tidak bergantung pada sesi.
	api.PUT("/uploads/:id/content", s.contentHandler)
	mutations := api.Group("")
	if sessions != nil {
		mutations.Use(httpapi.RequireOrigin(origin))
	}
	mutations.POST("/uploads/presign", sessions.WithAccount(s.presignHandler))
	mutations.POST("/uploads/complete", sessions.WithAccount(s.completeHandler))
}

func validatePresign(input *presignInput) (int, fieldErrors) {
	fields := make(fieldErrors)
	switch input.Purpose {
	case "product_photo", "event_banner":
	case "dispute_evidence":
		fields["purpose"] = "Unggah bukti sengketa belum tersedia."
	default:
		fields["purpose"] = "Pilih tujuan unggahan yang valid."
	}
	input.ContentType = strings.ToLower(strings.TrimSpace(input.ContentType))
	if !allowedTypes[input.ContentType] {
		return http.StatusUnsupportedMediaType, fieldErrors{"content_type": "Gunakan gambar JPG, PNG, atau WebP."}
	}
	if input.SizeBytes > maxSize {
		return http.StatusRequestEntityTooLarge, fieldErrors{"size_bytes": "Ukuran gambar paling besar 5 MB."}
	}
	if input.SizeBytes < 1 {
		fields["size_bytes"] = "File kosong tidak bisa diunggah."
	}
	if !httpapi.ValidID(input.OwnerID) {
		fields["owner_id"] = "Pilih usaha atau organisasi pemilik file."
	} else {
		input.OwnerID = strings.ToLower(input.OwnerID)
	}
	if len(fields) == 0 {
		return 0, nil
	}
	return http.StatusUnprocessableEntity, fields
}

func (s Store) presignHandler(c *gin.Context, account auth.Account) {
	input, ok := httpapi.ReadJSON[presignInput](c, maxInput)
	if !ok {
		return
	}
	if status, fields := validatePresign(&input); fields != nil {
		code := "VALIDATION_FAILED"
		switch status {
		case http.StatusUnsupportedMediaType:
			code = "UNSUPPORTED_MEDIA_TYPE"
		case http.StatusRequestEntityTooLarge:
			code = "PAYLOAD_TOO_LARGE"
		}
		httpapi.Error(c, status, code, "Periksa file yang akan diunggah.", fields)
		return
	}
	intent, err := s.presign(c.Request.Context(), account.ID, input)
	if err != nil {
		writeError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	httpapi.Success(c, http.StatusCreated, intent)
}

func (s Store) contentHandler(c *gin.Context) {
	if s.DB == nil {
		httpapi.Error(c, http.StatusServiceUnavailable, "UPLOADS_UNAVAILABLE", "Unggahan belum tersedia.", nil)
		return
	}
	id := strings.ToLower(c.Param("id"))
	if !httpapi.ValidID(id) || !s.verify(id, c.Query("expires"), c.Query("signature"), time.Now()) {
		writeError(c, errBadToken)
		return
	}
	if c.Request.ContentLength > maxSize {
		writeError(c, errTooLarge)
		return
	}
	body := http.MaxBytesReader(c.Writer, c.Request.Body, maxSize+1)
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(c.GetHeader("Content-Type"), ";")[0]))
	if err := s.receive(c.Request.Context(), id, contentType, body); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			err = errTooLarge
		}
		writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

type completeInput struct {
	UploadID string `json:"upload_id"`
}

func (s Store) completeHandler(c *gin.Context, account auth.Account) {
	input, ok := httpapi.ReadJSON[completeInput](c, maxInput)
	if !ok {
		return
	}
	if !httpapi.ValidID(input.UploadID) {
		writeError(c, fieldErrors{"upload_id": "Gunakan ID unggahan dari izin unggah."})
		return
	}
	status, err := s.complete(c.Request.Context(), account.ID, strings.ToLower(input.UploadID))
	if err != nil {
		writeError(c, err)
		return
	}
	httpapi.Success(c, http.StatusOK, status)
}

func (s Store) serve(c *gin.Context, upload readyUpload, cache string) {
	file, err := s.Storage.Open(upload.key)
	if err != nil {
		writeError(c, errNotFound)
		return
	}
	defer func() { _ = file.Close() }()
	c.Header("Content-Type", upload.contentType)
	c.Header("Content-Length", strconv.FormatInt(upload.size, 10))
	c.Header("Cache-Control", cache)
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Content-Disposition", "inline")
	c.Header("Content-Security-Policy", "default-src 'none'; sandbox")
	c.Status(http.StatusOK)
	_, _ = io.Copy(c.Writer, file)
}

func (s Store) fileHandler(c *gin.Context, account auth.Account) {
	id := strings.ToLower(c.Param("id"))
	if !httpapi.ValidID(id) {
		writeError(c, errNotFound)
		return
	}
	upload, err := s.readable(c.Request.Context(), account.ID, id)
	if err != nil {
		writeError(c, err)
		return
	}
	// ID unggahan tidak pernah dipakai ulang untuk isi lain, sehingga aman disimpan lama di browser pengguna.
	s.serve(c, upload, "private, max-age=86400, immutable")
}

func (s Store) publicHandler(c *gin.Context) {
	if s.DB == nil {
		httpapi.Error(c, http.StatusServiceUnavailable, "UPLOADS_UNAVAILABLE", "File belum tersedia.", nil)
		return
	}
	id := strings.ToLower(c.Param("id"))
	if !httpapi.ValidID(id) {
		writeError(c, errNotFound)
		return
	}
	upload, err := s.public(c.Request.Context(), id)
	if err != nil {
		writeError(c, err)
		return
	}
	s.serve(c, upload, "public, max-age=86400, immutable")
}

func writeError(c *gin.Context, err error) {
	var fields fieldErrors
	var reject rejected
	switch {
	case errors.As(err, &fields):
		httpapi.Error(c, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Periksa unggahan.", fields)
	case errors.As(err, &reject):
		httpapi.Error(c, http.StatusUnprocessableEntity, "UPLOAD_REJECTED", reject.reason, nil)
	case errors.Is(err, errNotFound):
		httpapi.Error(c, http.StatusNotFound, "NOT_FOUND", "Unggahan tidak ditemukan atau sudah kedaluwarsa.", nil)
	case errors.Is(err, errBadToken):
		httpapi.Error(c, http.StatusForbidden, "UPLOAD_URL_INVALID", "Tautan unggah tidak berlaku. Minta izin unggah baru.", nil)
	case errors.Is(err, errTooLarge):
		httpapi.Error(c, http.StatusRequestEntityTooLarge, "PAYLOAD_TOO_LARGE", "Ukuran file melebihi izin unggah.", nil)
	case errors.Is(err, errLimited):
		c.Header("Retry-After", "600")
		httpapi.Error(c, http.StatusTooManyRequests, "RATE_LIMITED", "Terlalu banyak unggahan yang belum selesai. Coba lagi nanti.", nil)
	case errors.Is(err, errNoContent):
		httpapi.Error(c, http.StatusConflict, "UPLOAD_EMPTY", "File belum diunggah ke tautan unggah.", nil)
	default:
		httpapi.LogInternalError(c, "upload request failed", err)
		httpapi.Error(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Unggahan belum dapat diproses.", nil)
	}
}
