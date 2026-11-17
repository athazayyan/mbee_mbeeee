package admin

import (
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"kitakarsa.com/tenantku/backend/internal/auth"
	"kitakarsa.com/tenantku/backend/internal/platform/adminperm"
	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

var feeCodePattern = regexp.MustCompile(`^[a-z0-9_]{1,40}$`)

var (
	bookingStatuses   = []string{"held", "payment_pending", "confirmed", "expired", "cancelled"}
	paymentStatuses   = []string{"created", "pending", "paid", "failed", "expired"}
	exceptionStatuses = []string{"open", "resolved"}
	eventStatuses     = []string{"draft", "published", "live", "completed", "cancelled"}
)

// readFilter reads the shared paging query plus an optional status that must be one of allowed.
func readFilter(c *gin.Context, allowed []string) (pageFilter, bool) {
	filter, fields := readPage(c)
	filter.Status = strings.TrimSpace(c.Query("status"))
	if filter.Status != "" && !slices.Contains(allowed, filter.Status) {
		fields["status"] = "Pilih salah satu: " + strings.Join(allowed, ", ") + "."
	}
	if len(fields) > 0 {
		httpapi.Error(c, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Periksa filter.", fields)
		return filter, false
	}
	return filter, true
}

func (s Store) listBookings(c *gin.Context, _ auth.Account) {
	filter, ok := readFilter(c, bookingStatuses)
	if !ok {
		return
	}
	data, err := s.bookings(c.Request.Context(), filter)
	if err != nil {
		writeError(c, err)
		return
	}
	page(c, data, filter.Limit-1, func(b BookingRow) string { return httpapi.EncodeCursor(b.CreatedAt, b.ID) })
}

func (s Store) listPayments(c *gin.Context, _ auth.Account) {
	filter, ok := readFilter(c, paymentStatuses)
	if !ok {
		return
	}
	data, err := s.payments(c.Request.Context(), filter)
	if err != nil {
		writeError(c, err)
		return
	}
	page(c, data, filter.Limit-1, func(p PaymentRow) string { return httpapi.EncodeCursor(p.CreatedAt, p.ID) })
}

func (s Store) listExceptions(c *gin.Context, _ auth.Account) {
	filter, ok := readFilter(c, exceptionStatuses)
	if !ok {
		return
	}
	data, err := s.exceptions(c.Request.Context(), filter)
	if err != nil {
		writeError(c, err)
		return
	}
	page(c, data, filter.Limit-1, func(x ExceptionRow) string { return httpapi.EncodeCursor(x.CreatedAt, x.ID) })
}

func (s Store) listEvents(c *gin.Context, _ auth.Account) {
	filter, ok := readFilter(c, eventStatuses)
	if !ok {
		return
	}
	data, err := s.events(c.Request.Context(), filter)
	if err != nil {
		writeError(c, err)
		return
	}
	page(c, data, filter.Limit-1, func(e EventRow) string { return httpapi.EncodeCursor(e.CreatedAt, e.ID) })
}

type reasonInput struct {
	Reason string `json:"reason"`
}

func readReason(c *gin.Context) (string, bool) {
	input, ok := httpapi.ReadJSON[reasonInput](c, maxInput)
	if !ok {
		return "", false
	}
	reason := strings.TrimSpace(input.Reason)
	if !validReason(reason) {
		httpapi.Error(c, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Periksa isian.",
			map[string]string{"reason": "Tulis alasan 1 sampai 500 karakter untuk jejak audit."})
		return "", false
	}
	return reason, true
}

func (s Store) resolveExceptionHandler(c *gin.Context, account auth.Account) {
	id := strings.ToLower(c.Param("id"))
	if !httpapi.ValidID(id) {
		writeError(c, pgx.ErrNoRows)
		return
	}
	reason, ok := readReason(c)
	if !ok {
		return
	}
	data, err := s.resolveException(c.Request.Context(), account.ID, id, reason, httpapi.RequestIDOf(c))
	if err != nil {
		writeError(c, err)
		return
	}
	httpapi.Success(c, http.StatusOK, data)
}

func (s Store) recheckHandler(c *gin.Context, account auth.Account) {
	id := strings.ToLower(c.Param("id"))
	if !httpapi.ValidID(id) {
		writeError(c, pgx.ErrNoRows)
		return
	}
	reason, ok := readReason(c)
	if !ok {
		return
	}
	data, err := s.recheckPayment(c.Request.Context(), account.ID, id, reason, httpapi.RequestIDOf(c))
	if err != nil {
		writeError(c, err)
		return
	}
	httpapi.Success(c, http.StatusOK, data)
}

func (s Store) listMethodFees(c *gin.Context, _ auth.Account) {
	data, err := s.methodFees(c.Request.Context())
	if err != nil {
		writeError(c, err)
		return
	}
	httpapi.Success(c, http.StatusOK, data)
}

func (s Store) updateMethodHandler(c *gin.Context, account auth.Account) {
	code := c.Param("code")
	if !feeCodePattern.MatchString(code) {
		writeError(c, pgx.ErrNoRows)
		return
	}
	input, ok := httpapi.ReadJSON[methodInput](c, maxInput)
	if !ok {
		return
	}
	if fields := validateMethod(&input); fields != nil {
		httpapi.Error(c, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Periksa isian tarif.", fields)
		return
	}
	data, err := s.updateMethod(c.Request.Context(), account.ID, code, input, httpapi.RequestIDOf(c))
	if err != nil {
		writeError(c, err)
		return
	}
	httpapi.Success(c, http.StatusOK, data)
}

func (s Store) listPlanVersions(c *gin.Context, _ auth.Account) {
	data, err := s.planVersions(c.Request.Context())
	if err != nil {
		writeError(c, err)
		return
	}
	httpapi.Success(c, http.StatusOK, data)
}

func (s Store) publishVersionHandler(c *gin.Context, account auth.Account) {
	input, ok := httpapi.ReadJSON[versionInput](c, 16<<10)
	if !ok {
		return
	}
	input.EffectiveAt = input.EffectiveAt.UTC().Truncate(time.Microsecond)
	if fields := validateVersion(&input, time.Now()); fields != nil {
		httpapi.Error(c, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Periksa isian harga paket.", fields)
		return
	}
	data, created, err := s.publishVersion(c.Request.Context(), account.ID, input, httpapi.RequestIDOf(c))
	if err != nil {
		writeError(c, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	httpapi.Success(c, status, data)
}

// withPermission rejects accounts that are not platform admins holding the permission.
func (s Store) withPermission(permission string, next func(*gin.Context, auth.Account)) func(*gin.Context, auth.Account) {
	return func(c *gin.Context, account auth.Account) {
		allowed, err := adminperm.Has(c.Request.Context(), s.DB, account.ID, permission)
		if err != nil {
			writeError(c, err)
			return
		}
		if !allowed {
			writeError(c, errForbidden)
			return
		}
		next(c, account)
	}
}

func (s Store) reverseLedgerHandler(c *gin.Context, account auth.Account) {
	id := strings.ToLower(c.Param("id"))
	if !httpapi.ValidID(id) {
		writeError(c, pgx.ErrNoRows)
		return
	}
	reason, ok := readReason(c)
	if !ok {
		return
	}
	data, created, err := s.reverseLedger(c.Request.Context(), account.ID, id, reason, httpapi.RequestIDOf(c))
	if err != nil {
		writeError(c, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	httpapi.Success(c, status, data)
}
