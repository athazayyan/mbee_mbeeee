package inventory

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"kitakarsa.com/tenantku/backend/internal/auth"
	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

func idempotencyKey(c *gin.Context) (string, bool) {
	key := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if !httpapi.ValidSingleLine(key, 128) || len(key) > 128 {
		httpapi.Error(c, http.StatusBadRequest, "INVALID_IDEMPOTENCY_KEY", "Sertakan Idempotency-Key dengan panjang 1–128 byte.", nil)
		return "", false
	}
	return key, true
}

func (s Store) createAdjustment(c *gin.Context, account auth.Account) {
	id := c.Param("id")
	if !validID(c, id) {
		return
	}
	key, ok := idempotencyKey(c)
	if !ok {
		return
	}
	input, ok := httpapi.ReadJSON[adjustmentInput](c, maxInput)
	if !ok {
		return
	}
	if fields := validateAdjustment(&input); fields != nil {
		writeError(c, fields)
		return
	}
	result, created, err := s.adjustSale(c.Request.Context(), account.ID, strings.ToLower(id), key, input)
	if err != nil {
		writeError(c, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	httpapi.Success(c, status, result)
}

func (s Store) postExpense(c *gin.Context, account auth.Account) {
	business := c.Param("id")
	if !validID(c, business) {
		return
	}
	key, ok := idempotencyKey(c)
	if !ok {
		return
	}
	input, ok := httpapi.ReadJSON[expenseInput](c, maxInput)
	if !ok {
		return
	}
	if fields := validateExpense(&input, time.Now()); fields != nil {
		writeError(c, fields)
		return
	}
	expense, created, err := s.createExpense(c.Request.Context(), account.ID, strings.ToLower(business), key, input)
	if err != nil {
		writeError(c, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	httpapi.Success(c, status, expense)
}

func (s Store) getExpenses(c *gin.Context, account auth.Account) {
	business := c.Param("id")
	if !validID(c, business) {
		return
	}
	fields := make(fieldErrors)
	limit, err := strconv.Atoi(c.DefaultQuery("limit", "50"))
	if err != nil || limit < 1 || limit > 100 {
		fields["limit"] = "Gunakan limit antara 1 dan 100."
	}
	filter := expenseFilter{Limit: limit + 1}
	for key, target := range map[string]*time.Time{"from": &filter.From, "to": &filter.To} {
		if raw := c.Query(key); raw != "" {
			parsed, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				fields[key] = "Gunakan waktu RFC 3339, misalnya 2026-12-01T00:00:00+07:00."
			}
			*target = parsed
		}
	}
	filter.AfterAt, filter.AfterID, err = httpapi.DecodeCursor(c.Query("cursor"))
	if err != nil {
		fields["cursor"] = "Gunakan cursor dari halaman sebelumnya."
	}
	if len(fields) > 0 {
		writeError(c, fields)
		return
	}
	data, total, err := s.listExpenses(c.Request.Context(), account.ID, business, filter)
	if err != nil {
		writeError(c, err)
		return
	}
	var next *string
	if len(data) > limit {
		data = data[:limit]
		last := data[len(data)-1]
		cursor := httpapi.EncodeCursor(last.OccurredAt, last.ID)
		next = &cursor
	}
	if data == nil {
		data = []Expense{}
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{"next_cursor": next, "total_amount": total}})
}
