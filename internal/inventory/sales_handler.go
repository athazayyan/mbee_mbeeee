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

const maxSyncBytes = 1 << 20

func (s Store) openPosSession(c *gin.Context, account auth.Account) {
	business := c.Param("id")
	if !validID(c, business) {
		return
	}
	input, ok := httpapi.ReadJSON[posSessionInput](c, maxInput)
	if !ok {
		return
	}
	if fields := validateSessionInput(&input); fields != nil {
		writeError(c, fields)
		return
	}
	session, created, err := s.openSession(c.Request.Context(), account.ID, business, input)
	if err != nil {
		writeError(c, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	httpapi.Success(c, status, session)
}

func (s Store) syncSales(c *gin.Context, account auth.Account) {
	if c.Request.ContentLength > maxSyncBytes {
		httpapi.Error(c, http.StatusRequestEntityTooLarge, "PAYLOAD_TOO_LARGE", "Kirim transaksi dalam batch yang lebih kecil.", nil)
		return
	}
	batch, ok := httpapi.ReadJSON[salesBatch](c, maxSyncBytes)
	if !ok {
		return
	}
	if len(batch.Sales) == 0 || len(batch.Sales) > maxSalesPerBatch {
		httpapi.Error(c, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Kirim 1 sampai 100 transaksi per batch.",
			map[string]string{"sales": "Kirim 1 sampai 100 transaksi per batch."})
		return
	}
	now := time.Now()
	results := make([]syncOutcome, 0, len(batch.Sales))
	for _, sale := range batch.Sales {
		if code := validateSale(&sale, now); code != "" {
			results = append(results, syncOutcome{SaleID: sale.SaleID, Status: "rejected", Code: string(code)})
			continue
		}
		results = append(results, s.syncSale(c.Request.Context(), account.ID, sale))
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"results": results}, "meta": gin.H{"server_time": time.Now().UTC()}})
}

func (s Store) listSalesHandler(c *gin.Context, account auth.Account) {
	business := c.Param("id")
	if !validID(c, business) {
		return
	}
	fields := make(fieldErrors)
	limit, err := strconv.Atoi(c.DefaultQuery("limit", "50"))
	if err != nil || limit < 1 || limit > 100 {
		fields["limit"] = "Gunakan limit antara 1 dan 100."
	}
	filter := salesFilter{Limit: limit + 1}
	for key, target := range map[string]*time.Time{"from": &filter.From, "to": &filter.To} {
		if raw := c.Query(key); raw != "" {
			parsed, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				fields[key] = "Gunakan waktu RFC 3339, misalnya 2026-12-01T00:00:00+07:00."
			}
			*target = parsed
		}
	}
	if !filter.From.IsZero() && !filter.To.IsZero() && !filter.To.After(filter.From) {
		fields["to"] = "Batas akhir harus setelah batas awal."
	}
	if event := c.Query("event_id"); event != "" {
		if !httpapi.ValidID(event) {
			fields["event_id"] = "Pilih acara yang valid."
		}
		event = strings.ToLower(event)
		filter.EventID = &event
	}
	filter.AfterAt, filter.AfterID, err = httpapi.DecodeCursor(c.Query("cursor"))
	if err != nil {
		fields["cursor"] = "Gunakan cursor dari halaman sebelumnya."
	}
	if len(fields) > 0 {
		writeError(c, fields)
		return
	}
	data, err := s.listSales(c.Request.Context(), account.ID, business, filter)
	if err != nil {
		writeError(c, err)
		return
	}
	var next *string
	if len(data) > limit {
		data = data[:limit]
		last := data[len(data)-1]
		cursor := httpapi.EncodeCursor(last.ClientCreatedAt, last.ID)
		next = &cursor
	}
	if data == nil {
		data = []Sale{}
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": gin.H{"next_cursor": next}})
}
