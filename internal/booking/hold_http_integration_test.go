package booking

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"kitakarsa.com/tenantku/backend/internal/auth"
	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

func TestHoldHTTPContention(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store := migratedBookingStore(t, ctx)
	service, err := auth.New(ctx, store.DB, strings.Repeat("s", 32), strings.Repeat("r", 32))
	if err != nil {
		t.Fatal(err)
	}
	const origin = "http://tenantku.test"
	router := gin.New()
	router.Use(httpapi.RequestID)
	api := router.Group("/api/v1")
	service.Routes(api, origin)
	store.Routes(api, service)
	settings := HoldSettings{Lifetime: 15 * time.Minute, MaxActive: 2}
	if err := store.HoldRoutes(api, service, origin, settings, nil); err != ErrHoldUnavailable {
		t.Fatalf("unconfigured registration: %v", err)
	}
	offers := testOffers{Offer{
		PriceRevision: 1, Rent: 30_000, PlatformFee: 1_000, ProviderFee: 500,
		PolicyVersion: "test-policy", RefundPolicy: json.RawMessage(`{"test":true}`),
	}}
	if err := store.HoldRoutes(api, service, origin, settings, offers); err != nil {
		t.Fatal(err)
	}

	send := func(method, path, from, key string, data []byte, cookies []*http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(ctx, method, "/api/v1"+path, bytes.NewReader(data))
		req.Header.Set("Origin", from)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", key)
		for _, cookie := range cookies {
			req.AddCookie(cookie)
		}
		reply := httptest.NewRecorder()
		router.ServeHTTP(reply, req)
		return reply
	}
	assertStatus := func(reply *httptest.ResponseRecorder, status int, code string) {
		t.Helper()
		if reply.Code != status || code != "" && !strings.Contains(reply.Body.String(), `"code":"`+code+`"`) {
			t.Fatalf("expected %d %s, got %d %s", status, code, reply.Code, reply.Body)
		}
	}

	category := insertID(t, ctx, store, `SELECT id FROM business_categories ORDER BY id LIMIT 1`)
	type tenant struct {
		actor, business string
		cookies         []*http.Cookie
	}
	tenants := make([]tenant, 5)
	for i := range tenants {
		credentials, err := json.Marshal(map[string]string{
			"email": fmt.Sprintf("hold-%d@example.com", i), "password": "test-only-long-password",
		})
		if err != nil {
			t.Fatal(err)
		}
		registered := send(http.MethodPost, "/auth/register", origin, "", credentials, nil)
		assertStatus(registered, http.StatusCreated, "")
		var account struct{ Data auth.Account }
		if err := json.Unmarshal(registered.Body.Bytes(), &account); err != nil {
			t.Fatal(err)
		}
		actor := account.Data.ID
		business := insertID(t, ctx, store, `INSERT INTO businesses (owner_id, category_id, name)
			VALUES ($1, $2, 'Business') RETURNING id`, actor, category)
		execBookingSQL(t, ctx, store, `INSERT INTO memberships (user_id, business_id, role)
			VALUES ($1, $2, 'owner')`, actor, business)
		login := send(http.MethodPost, "/auth/login", origin, "", credentials, nil)
		assertStatus(login, http.StatusOK, "")
		tenants[i] = tenant{actor, business, login.Result().Cookies()}
	}

	organizer := tenants[0].actor
	org := insertID(t, ctx, store, `INSERT INTO organizations (owner_id, name) VALUES ($1, 'Sample') RETURNING id`, organizer)
	event := insertID(t, ctx, store, `INSERT INTO events
		(organization_id, created_by, name, slug, status, starts_at, ends_at, timezone, city, address, terms)
		VALUES ($1, $2, 'Event', 'event', 'published', now(), now() + interval '2 hours',
			'Asia/Jakarta', 'Banda Aceh', 'Street', 'Original terms') RETURNING id`, org, organizer)
	slot := insertID(t, ctx, store, `INSERT INTO event_slots (event_id, code, starts_at, ends_at)
		SELECT id, 'whole-event', starts_at, ends_at FROM events WHERE id = $1 RETURNING id`, event)
	execBookingSQL(t, ctx, store, `INSERT INTO event_pricing_versions (event_id, revision, created_by, rules, effective_at)
		VALUES ($1, 1, $2, '{"default_base_price":30000}', now())`, event, organizer)

	input := func(tenant tenant, stand, policy string) []byte {
		body, err := json.Marshal(HoldInput{tenant.business, event, stand, slot, policy})
		if err != nil {
			panic(err)
		}
		return body
	}
	const route = "/bookings/holds"
	first, second := tenants[0], tenants[1]
	body := input(first, testStandOne, "test-policy")
	assertStatus(send(http.MethodPost, route, origin, "auth", body, nil), http.StatusUnauthorized, "")
	assertStatus(send(http.MethodPost, route, "https://foreign.example", "origin", body, first.cookies), http.StatusForbidden, "ORIGIN_REJECTED")
	assertStatus(send(http.MethodPost, route, origin, "", body, first.cookies), http.StatusBadRequest, "INVALID_IDEMPOTENCY_KEY")
	assertStatus(send(http.MethodPost, route, origin, "ownership", body, second.cookies), http.StatusNotFound, "NOT_FOUND")
	assertStatus(send(http.MethodPost, route, origin, "policy", input(first, testStandOne, "old-policy"), first.cookies), http.StatusConflict, "REFUND_POLICY_CHANGED")
	assertStatus(send(http.MethodPost, route, origin, "invalid", []byte(`{"unexpected":1}`), first.cookies), http.StatusUnprocessableEntity, "VALIDATION_FAILED")

	const contenders = 50
	type raceResult struct {
		tenant tenant
		key    string
		reply  *httptest.ResponseRecorder
	}
	start := make(chan struct{})
	results := make(chan raceResult, contenders)
	for i := range contenders {
		tenant := tenants[i%len(tenants)]
		key := fmt.Sprintf("race-%d", i)
		body := input(tenant, testStandOne, "test-policy")
		go func() {
			<-start
			results <- raceResult{tenant, key, send(http.MethodPost, route, origin, key, body, tenant.cookies)}
		}()
	}
	close(start)
	var winners []raceResult
	for range contenders {
		result := <-results
		if result.reply.Code == http.StatusCreated {
			winners = append(winners, result)
			continue
		}
		assertStatus(result.reply, http.StatusConflict, "STAND_UNAVAILABLE")
	}
	if len(winners) != 1 {
		t.Fatalf("%d winners for %d requests", len(winners), contenders)
	}
	winner := winners[0]
	type holdResponse struct {
		Data Booking
		Meta struct {
			Now time.Time `json:"server_now"`
		}
	}
	decode := func(reply *httptest.ResponseRecorder) holdResponse {
		t.Helper()
		var result holdResponse
		if err := json.Unmarshal(reply.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	created := decode(winner.reply)
	if created.Data.Status != "held" || created.Data.Price.Total != 31_500 ||
		created.Data.PolicyVersion != "test-policy" || created.Meta.Now.IsZero() || !created.Data.ExpiresAt.After(created.Meta.Now) {
		t.Fatalf("unexpected hold: %+v", created)
	}
	var counts [3]int
	err = store.DB.QueryRow(ctx, `SELECT (SELECT count(*) FROM bookings),
		(SELECT count(*) FROM allocations WHERE status = 'hold'), (SELECT count(*) FROM booking_status_history)`).
		Scan(&counts[0], &counts[1], &counts[2])
	if err != nil || counts != [3]int{1, 1, 1} {
		t.Fatalf("race produced counts %v, error %v", counts, err)
	}
	cookies := winner.tenant.cookies
	replay := func(key, stand string) *httptest.ResponseRecorder {
		return send(http.MethodPost, route, origin, key, input(winner.tenant, stand, "test-policy"), cookies)
	}
	retried := replay(winner.key, testStandOne)
	assertStatus(retried, http.StatusOK, "")
	if replayed := decode(retried); replayed.Data.ID != created.Data.ID || replayed.Data.Price != created.Data.Price {
		t.Fatalf("retry changed hold: %+v", replayed)
	}
	assertStatus(replay(winner.key, testStandTwo), http.StatusConflict, "IDEMPOTENCY_PAYLOAD_MISMATCH")
	assertStatus(replay("second-hold", testStandTwo), http.StatusCreated, "")
	assertStatus(replay("limit-reached", testStandThree), http.StatusTooManyRequests, "HOLD_LIMIT_REACHED")
	execBookingSQL(t, ctx, store, `UPDATE bookings SET expires_at = now() - interval '1 second' WHERE id = $1`, created.Data.ID)
	retried = replay(winner.key, testStandOne)
	assertStatus(retried, http.StatusOK, "")
	if replayed := decode(retried); replayed.Data.Status != "expired" || replayed.Data.ID != created.Data.ID {
		t.Fatalf("expired retry changed hold: %+v", replayed)
	}
	other := first
	if other.actor == winner.tenant.actor {
		other = second
	}
	assertStatus(send(http.MethodGet, "/bookings/"+created.Data.ID, origin, "", nil, other.cookies), http.StatusNotFound, "NOT_FOUND")
}
