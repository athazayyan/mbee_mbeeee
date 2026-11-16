package auth

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/mail"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

const (
	accessCookie  = "tenantku_access"
	refreshCookie = "tenantku_refresh"
)

type handler struct {
	service *Service
	secure  bool
}

func (s *Service) Routes(api *gin.RouterGroup, origin string) {
	h := handler{service: s, secure: strings.HasPrefix(origin, "https://")}
	group := api.Group("/auth", httpapi.RequireOrigin(origin), limitMutations())
	group.POST("/register", h.register)
	group.POST("/login", h.login)
	group.POST("/refresh", h.refresh)
	group.POST("/logout", h.logout)
}

// Separate minute windows and cap the number of peers tracked per process.
func limitMutations() gin.HandlerFunc {
	var mu sync.Mutex
	var window time.Time
	counts := make(map[string]int)

	return func(c *gin.Context) {
		peer := c.ClientIP()
		minute := time.Now().Truncate(time.Minute)
		mu.Lock()
		if minute != window {
			clear(counts)
			window = minute
		}
		used, known := counts[peer]
		allowed := used < 30 && (known || len(counts) < 4096)
		if allowed {
			counts[peer]++
		}
		mu.Unlock()

		if !allowed {
			c.Header("Retry-After", "60")
			httpapi.Error(c, http.StatusTooManyRequests, "RATE_LIMITED", "Terlalu banyak percobaan. Coba lagi nanti.", nil)
		}
	}
}

type credentials struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func readCredentials(c *gin.Context) (credentials, bool) {
	var input credentials
	body := http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || decoder.Decode(new(json.RawMessage)) != io.EOF {
		httpapi.Error(c, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Payload akun tidak valid.", nil)
		return input, false
	}
	input.Email = strings.ToLower(strings.TrimSpace(input.Email))
	fields := make(map[string]string)
	address, err := mail.ParseAddress(input.Email)
	if err != nil || address.Address != input.Email || len(input.Email) > 254 {
		fields["email"] = "Masukkan alamat email yang valid."
	}
	if utf8.RuneCountInString(input.Password) < 12 || len(input.Password) > 72 {
		fields["password"] = "Gunakan minimal 12 karakter dan maksimal 72 byte."
	}
	if len(fields) != 0 {
		httpapi.Error(c, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Periksa email dan kata sandi.", fields)
		return input, false
	}
	return input, true
}

func (h handler) register(c *gin.Context) {
	input, ok := readCredentials(c)
	if !ok {
		return
	}
	account, err := h.service.register(c.Request.Context(), input.Email, input.Password)
	if err != nil {
		sendError(c, err)
		return
	}
	httpapi.Success(c, http.StatusCreated, account)
}

func (h handler) login(c *gin.Context) {
	input, ok := readCredentials(c)
	if !ok {
		return
	}
	account, result, err := h.service.login(c.Request.Context(), input.Email, input.Password)
	if err != nil {
		sendError(c, err)
		return
	}
	h.writeSession(c, result)
	httpapi.Success(c, http.StatusOK, account)
}

func (h handler) refresh(c *gin.Context) {
	previous, err := c.Cookie(refreshCookie)
	if err != nil {
		sendError(c, errSession)
		return
	}
	account, result, err := h.service.refresh(c.Request.Context(), previous)
	if err != nil {
		if errors.Is(err, errSession) {
			h.clearSession(c)
		}
		sendError(c, err)
		return
	}
	h.writeSession(c, result)
	httpapi.Success(c, http.StatusOK, account)
}

func (h handler) logout(c *gin.Context) {
	if refresh, err := c.Cookie(refreshCookie); err == nil {
		if err := h.service.logout(c.Request.Context(), refresh); err != nil {
			sendError(c, err)
			return
		}
	}
	h.clearSession(c)
	c.Status(http.StatusNoContent)
}

func (s *Service) WithAccount(next func(*gin.Context, Account)) gin.HandlerFunc {
	return func(c *gin.Context) {
		if s == nil {
			httpapi.Error(c, http.StatusServiceUnavailable, "AUTH_UNAVAILABLE", "Layanan akun belum tersedia.", nil)
			return
		}
		access, err := c.Cookie(accessCookie)
		if err != nil {
			sendError(c, errSession)
			return
		}
		account, err := s.account(c.Request.Context(), access)
		if err != nil {
			sendError(c, err)
			return
		}
		next(c, account)
	}
}

func (h handler) writeSession(c *gin.Context, result tokens) {
	h.setCookie(c, accessCookie, result.access, "/api/v1", int(accessTTL.Seconds()))
	h.setCookie(c, refreshCookie, result.refresh, "/api/v1/auth", max(1, int(time.Until(result.expires).Seconds())))
}

func (h handler) clearSession(c *gin.Context) {
	h.setCookie(c, accessCookie, "", "/api/v1", -1)
	h.setCookie(c, refreshCookie, "", "/api/v1/auth", -1)
}

func (h handler) setCookie(c *gin.Context, name, value, path string, maxAge int) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name: name, Value: value, Path: path, MaxAge: maxAge,
		Secure: h.secure, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
}

func sendError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, errConflict):
		httpapi.Error(c, http.StatusConflict, "ACCOUNT_CONFLICT", "Akun tidak dapat dibuat dengan email ini.", nil)
	case errors.Is(err, errSession):
		httpapi.Error(c, http.StatusUnauthorized, "INVALID_SESSION", "Kredensial atau sesi tidak valid.", nil)
	default:
		httpapi.LogInternalError(c, "auth request failed", err)
		httpapi.Error(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Terjadi kesalahan. Coba lagi nanti.", nil)
	}
}
