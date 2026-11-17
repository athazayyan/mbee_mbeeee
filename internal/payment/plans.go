package payment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"kitakarsa.com/tenantku/backend/internal/auth"
	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

// planPaymentWindow is how long an organizer has to pay for a plan before the Snap order lapses.
const planPaymentWindow = 24 * time.Hour

var (
	ErrPlanUnknown = errors.New("plan does not exist or is not on sale")
	ErrPlanState   = errors.New("event cannot buy this plan right now")
)

type Plan struct {
	Code           string `json:"code"`
	Name           string `json:"name"`
	Price          int64  `json:"price"`
	Currency       string `json:"currency"`
	CapacityStands *int   `json:"capacity_stands"`
}

type PlanPurchase struct {
	ID             string     `json:"id"`
	EventID        string     `json:"event_id"`
	PlanCode       string     `json:"plan_code"`
	Status         string     `json:"status"`
	Amount         int64      `json:"amount"`
	CapacityStands *int       `json:"capacity_stands"`
	ActivatedAt    *time.Time `json:"activated_at,omitempty"`
}

type PlanState struct {
	Required bool          `json:"required"`
	Active   bool          `json:"active"`
	Purchase *PlanPurchase `json:"purchase"`
	Payment  *Attempt      `json:"payment"`
}

type PlanCheckout struct {
	Purchase PlanPurchase `json:"purchase"`
	Payment  *Attempt     `json:"payment,omitempty"`
}

type PlanInput struct {
	PlanCode       string `json:"plan_code"`
	Method         string `json:"method"`
	CapacityStands *int   `json:"capacity_stands,omitempty"`
}

const purchaseColumns = `p.id, p.event_id, p.plan_code, p.status, p.amount, p.capacity_stands, p.activated_at`

func scanPurchase(row pgx.Row) (PlanPurchase, error) {
	var p PlanPurchase
	err := row.Scan(&p.ID, &p.EventID, &p.PlanCode, &p.Status, &p.Amount, &p.CapacityStands, &p.ActivatedAt)
	return p, err
}

func (s Store) ListPlans(ctx context.Context) ([]Plan, error) {
	rows, err := s.DB.Query(ctx, `SELECT code, name, price, capacity_stands FROM event_plans WHERE active ORDER BY price, code`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	plans := make([]Plan, 0)
	for rows.Next() {
		p := Plan{Currency: "IDR"}
		if err := rows.Scan(&p.Code, &p.Name, &p.Price, &p.CapacityStands); err != nil {
			return nil, err
		}
		plans = append(plans, p)
	}
	return plans, rows.Err()
}

// PlanStatus is visible to any member of the organization that owns the event.
func (s Store) PlanStatus(ctx context.Context, actor, eventID string) (PlanState, error) {
	state := PlanState{Required: s.PlanRequired}
	if !httpapi.ValidID(actor) || !httpapi.ValidID(eventID) {
		return state, pgx.ErrNoRows
	}
	err := pgx.BeginTxFunc(ctx, s.DB, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var visible string
		if err := tx.QueryRow(ctx, `SELECT e.id FROM events e JOIN memberships m ON m.organization_id = e.organization_id
			WHERE e.id = $1 AND m.user_id = $2`, eventID, actor).Scan(&visible); err != nil {
			return err
		}
		// Prefer the active purchase; otherwise show the open one so checkout can resume.
		p, err := scanPurchase(tx.QueryRow(ctx, `SELECT `+purchaseColumns+` FROM event_plan_purchases p
			WHERE p.event_id = $1 ORDER BY (p.status = 'active') DESC, p.created_at DESC LIMIT 1`, eventID))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		state.Purchase, state.Active = &p, p.Status == "active"
		if state.Active {
			return nil
		}
		a, err := scanAttempt(tx.QueryRow(ctx, `SELECT `+attemptColumns+` FROM payment_attempts
			WHERE purchase_id = $1 AND status IN ('created', 'pending')`, p.ID))
		if err == nil {
			state.Payment = &a
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		return nil
	})
	return state, err
}

// PurchasePlan starts (or resumes) payment for an event plan. The price always comes from event_plans.
// A free plan activates immediately and creates no payment attempt.
func (s Store) PurchasePlan(ctx context.Context, actor, email, eventID, key string, input PlanInput) (PlanCheckout, bool, error) {
	input.PlanCode, input.Method = strings.ToLower(strings.TrimSpace(input.PlanCode)), strings.ToLower(strings.TrimSpace(input.Method))
	if !httpapi.ValidID(actor) || !httpapi.ValidID(eventID) {
		return PlanCheckout{}, false, pgx.ErrNoRows
	}
	digest := sha256.Sum256([]byte("plan\x00" + eventID + "\x00" + input.PlanCode + "\x00" + input.Method))
	if input.PlanCode == "custom" {
		if input.CapacityStands == nil || *input.CapacityStands < 101 || *input.CapacityStands > 500 {
			return PlanCheckout{}, false, ErrPlanUnknown
		}
		digest = sha256.Sum256(strconv.AppendInt(append(digest[:], 0), int64(*input.CapacityStands), 10))
	} else if input.CapacityStands != nil {
		return PlanCheckout{}, false, ErrPlanUnknown
	}
	var result PlanCheckout
	var created bool
	err := pgx.BeginTxFunc(ctx, s.DB, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(
			hashtextextended('tenantku.payment.attempt:' || $1::uuid::text, 0))`, actor); err != nil {
			return err
		}
		var status string
		if err := tx.QueryRow(ctx, `SELECT e.status FROM events e JOIN memberships m ON m.organization_id = e.organization_id
			WHERE e.id = $1 AND m.user_id = $2 AND m.role = 'owner' FOR NO KEY UPDATE OF e`, eventID, actor).Scan(&status); err != nil {
			return err
		}

		var prior []byte
		var priorPurchase *string
		var priorEvent string
		err := tx.QueryRow(ctx, `SELECT a.request_digest, a.purchase_id, COALESCE(p.event_id::text, '') FROM payment_attempts a
			LEFT JOIN event_plan_purchases p ON p.id = a.purchase_id
			WHERE a.created_by = $1 AND a.idempotency_key = $2`, actor, key).Scan(&prior, &priorPurchase, &priorEvent)
		if err == nil && (!bytes.Equal(prior, digest[:]) || priorPurchase == nil || priorEvent != eventID) {
			return ErrIdempotencyMismatch
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil {
			// Replay: report the stored purchase and attempt without creating another.
			purchase, err := scanPurchase(tx.QueryRow(ctx, `SELECT `+purchaseColumns+` FROM event_plan_purchases p WHERE p.id = $1`, *priorPurchase))
			if err != nil {
				return err
			}
			attempt, err := scanAttempt(tx.QueryRow(ctx, `SELECT `+attemptColumns+` FROM payment_attempts
				WHERE created_by = $1 AND idempotency_key = $2`, actor, key))
			if err != nil {
				return err
			}
			result = PlanCheckout{Purchase: purchase, Payment: &attempt}
			return nil
		}

		if active, err := scanPurchase(tx.QueryRow(ctx, `SELECT `+purchaseColumns+` FROM event_plan_purchases p
			WHERE p.event_id = $1 AND p.status = 'active'`, eventID)); err == nil {
			result = PlanCheckout{Purchase: active}
			return nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if status != "draft" && status != "published" && status != "live" {
			return ErrPlanState
		}
		var price int64
		var capacity *int
		if err := tx.QueryRow(ctx, `SELECT price, capacity_stands FROM event_plans WHERE code = $1 AND active`,
			input.PlanCode).Scan(&price, &capacity); errors.Is(err, pgx.ErrNoRows) {
			return ErrPlanUnknown
		} else if err != nil {
			return err
		}
		if input.PlanCode == "custom" {
			capacity = input.CapacityStands
			price += int64(*capacity-100) * 5000
		}

		purchase, err := scanPurchase(tx.QueryRow(ctx, `SELECT `+purchaseColumns+` FROM event_plan_purchases p
			WHERE p.event_id = $1 AND p.status = 'pending' FOR UPDATE`, eventID))
		if errors.Is(err, pgx.ErrNoRows) {
			purchase, err = scanPurchase(tx.QueryRow(ctx, `INSERT INTO event_plan_purchases AS p
				(event_id, plan_code, purchased_by, amount, capacity_stands) VALUES ($1, $2, $3, $4, $5)
				RETURNING `+purchaseColumns, eventID, input.PlanCode, actor, price, capacity))
		} else if err == nil && (purchase.PlanCode != input.PlanCode ||
			(input.PlanCode == "custom" && (purchase.CapacityStands == nil || *purchase.CapacityStands != *capacity))) {
			var live int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM payment_attempts WHERE purchase_id = $1
				AND status IN ('created', 'pending')`, purchase.ID).Scan(&live); err != nil {
				return err
			}
			if live > 0 {
				return ErrPlanState // finish or wait out the open payment before switching plans
			}
			purchase, err = scanPurchase(tx.QueryRow(ctx, `UPDATE event_plan_purchases p SET plan_code = $2, amount = $3,
				capacity_stands = $4 WHERE p.id = $1 RETURNING `+purchaseColumns, purchase.ID, input.PlanCode, price, capacity))
		}
		if err != nil {
			return err
		}
		result.Purchase, created = purchase, true

		if purchase.Amount == 0 {
			result.Purchase, err = scanPurchase(tx.QueryRow(ctx, `UPDATE event_plan_purchases p SET status = 'active',
				activated_at = clock_timestamp() WHERE p.id = $1 RETURNING `+purchaseColumns, purchase.ID))
			return err
		}
		if input.Method != "snap" {
			return ErrPlanUnknown
		}
		live, err := scanAttempt(tx.QueryRow(ctx, `SELECT `+attemptColumns+` FROM payment_attempts
			WHERE purchase_id = $1 AND status IN ('created', 'pending')`, purchase.ID))
		if err == nil {
			result.Payment, created = &live, false
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		attempt, err := scanAttempt(tx.QueryRow(ctx, `INSERT INTO payment_attempts
			(purchase_id, created_by, provider, method, status, amount, expires_at, idempotency_key, request_digest)
			VALUES ($1, $2, 'midtrans', $3, 'created', $4, clock_timestamp() + $5::interval, $6, $7) RETURNING `+attemptColumns,
			purchase.ID, actor, input.Method, purchase.Amount, planPaymentWindow.String(), key, digest[:]))
		if err != nil {
			return err
		}
		result.Payment = &attempt
		return nil
	})
	if err != nil {
		return PlanCheckout{}, false, err
	}
	if result.Payment != nil && result.Payment.Status == "created" {
		if s.Provider == nil {
			return PlanCheckout{}, false, ErrUnavailable
		}
		opened, oerr := s.openPlanAttempt(ctx, result.Payment.ID, email)
		if oerr != nil {
			return PlanCheckout{}, false, oerr
		}
		result.Payment = &opened
	}
	return result, created, nil
}

func (s Store) openPlanAttempt(ctx context.Context, id, email string) (Attempt, error) {
	var result Attempt
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		attempt, err := scanAttempt(tx.QueryRow(ctx, `SELECT `+attemptColumns+` FROM payment_attempts WHERE id = $1 FOR UPDATE`, id))
		if err != nil {
			return err
		}
		if attempt.Status != "created" {
			result = attempt
			return nil
		}
		var eventName, planName, purchaseID, eventID, organizationID string
		var remaining float64
		if err := tx.QueryRow(ctx, `SELECT e.name, pl.name, p.id, e.id, e.organization_id, EXTRACT(EPOCH FROM (a.expires_at - clock_timestamp()))::float8
			FROM payment_attempts a JOIN event_plan_purchases p ON p.id = a.purchase_id
			JOIN events e ON e.id = p.event_id JOIN event_plans pl ON pl.code = p.plan_code
			WHERE a.id = $1 AND p.status = 'pending'`, id).Scan(&eventName, &planName, &purchaseID, &eventID, &organizationID, &remaining); errors.Is(err, pgx.ErrNoRows) {
			return ErrPlanState
		} else if err != nil {
			return err
		}
		if remaining <= 0 {
			return ErrPlanState
		}
		name := planName + " - " + eventName
		if len([]rune(name)) > 50 {
			name = string([]rune(name)[:50])
		}
		snap, err := s.Provider.CreateSnap(ctx, SnapInput{
			OrderID: id, Amount: attempt.Amount, Email: email,
			Items:     []snapItem{{ID: purchaseID, Price: attempt.Amount, Quantity: 1, Name: name}},
			Expiry:    time.Duration(remaining * float64(time.Second)),
			FinishURL: s.finishURL("/penyelenggara/acara/" + eventID + "/paket?organisasi=" + organizationID),
		})
		if err != nil {
			return err
		}
		result, err = scanAttempt(tx.QueryRow(ctx, `UPDATE payment_attempts SET status = 'pending', provider_token = $2,
			checkout_url = $3, updated_at = clock_timestamp() WHERE id = $1 RETURNING `+attemptColumns, id, snap.Token, snap.RedirectURL))
		return err
	})
	return result, err
}

// applyPlan applies a verified provider status to a plan payment. Paid activates the plan once and
// books the amount as platform revenue; a payment that cannot activate is queued as an exception.
func (s Store) applyPlan(ctx context.Context, tx pgx.Tx, attemptID, purchaseID, status string, amount int64,
	next string, exception exceptionFn) (string, error) {
	switch next {
	case "pending":
		if status == "created" {
			_, err := tx.Exec(ctx, `UPDATE payment_attempts SET status = 'pending', updated_at = clock_timestamp() WHERE id = $1`, attemptID)
			return "applied", err
		}
		return "applied", nil
	case "failed", "expired":
		if status != "created" && status != "pending" {
			return "ignored", nil
		}
		// The purchase stays pending so the organizer can retry with a new attempt.
		_, err := tx.Exec(ctx, `UPDATE payment_attempts SET status = $2, updated_at = clock_timestamp() WHERE id = $1`, attemptID, next)
		return "applied", err
	}
	if _, err := tx.Exec(ctx, `UPDATE payment_attempts SET status = 'paid', paid_at = clock_timestamp(),
		updated_at = clock_timestamp() WHERE id = $1`, attemptID); err != nil {
		return "", err
	}
	var purchaseStatus, eventID string
	if err := tx.QueryRow(ctx, `SELECT status, event_id FROM event_plan_purchases WHERE id = $1 FOR UPDATE`, purchaseID).
		Scan(&purchaseStatus, &eventID); err != nil {
		return "", err
	}
	if purchaseStatus != "pending" {
		return "applied", exception("duplicate_payment", &attemptID, nil, map[string]any{"amount": amount, "purchase_id": purchaseID})
	}
	var other bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM event_plan_purchases
		WHERE event_id = $1 AND status = 'active')`, eventID).Scan(&other); err != nil {
		return "", err
	}
	if other {
		return "applied", exception("duplicate_payment", &attemptID, nil, map[string]any{"amount": amount, "purchase_id": purchaseID})
	}
	if _, err := tx.Exec(ctx, `UPDATE event_plan_purchases SET status = 'active', activated_at = clock_timestamp() WHERE id = $1`, purchaseID); err != nil {
		return "", err
	}
	return "applied", recordPlanPayment(ctx, tx, attemptID, amount)
}

func (s Store) planRoutes(api *gin.RouterGroup, sessions *auth.Service, origin string) {
	unavailable := func(c *gin.Context) bool {
		if s.DB == nil {
			httpapi.Error(c, http.StatusServiceUnavailable, "PAYMENT_UNAVAILABLE", "Paket belum tersedia.", nil)
			return true
		}
		return false
	}
	api.GET("/plans", sessions.WithAccount(func(c *gin.Context, _ auth.Account) {
		if unavailable(c) {
			return
		}
		plans, err := s.ListPlans(c.Request.Context())
		if err != nil {
			httpapi.LogInternalError(c, "list plans failed", err)
			httpapi.Error(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Paket belum dapat dimuat.", nil)
			return
		}
		httpapi.Success(c, http.StatusOK, plans)
	}))
	api.GET("/events/:id/plan", sessions.WithAccount(func(c *gin.Context, account auth.Account) {
		if unavailable(c) {
			return
		}
		state, err := s.PlanStatus(c.Request.Context(), account.ID, c.Param("id"))
		if err != nil {
			writePlanError(c, err)
			return
		}
		httpapi.Success(c, http.StatusOK, state)
	}))
	api.POST("/events/:id/plan/purchases", httpapi.RequireOrigin(origin), sessions.WithAccount(func(c *gin.Context, account auth.Account) {
		if unavailable(c) {
			return
		}
		key := c.GetHeader("Idempotency-Key")
		if len(key) < 1 || len(key) > 128 {
			httpapi.Error(c, http.StatusBadRequest, "INVALID_IDEMPOTENCY_KEY", "Sertakan Idempotency-Key dengan panjang 1–128 byte.", nil)
			return
		}
		input, ok := httpapi.ReadJSON[PlanInput](c, 2048)
		if !ok {
			return
		}
		result, created, err := s.PurchasePlan(c.Request.Context(), account.ID, account.Email, c.Param("id"), key, input)
		if err != nil {
			writePlanError(c, err)
			return
		}
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		c.JSON(status, gin.H{"data": result, "meta": gin.H{}})
	}))
}

func writePlanError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		httpapi.Error(c, http.StatusNotFound, "NOT_FOUND", "Acara tidak ditemukan.", nil)
	case errors.Is(err, ErrPlanUnknown):
		httpapi.Error(c, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Paket atau metode pembayaran tidak tersedia.",
			map[string]string{"plan_code": "Pilih paket yang tersedia."})
	case errors.Is(err, ErrPlanState):
		httpapi.Error(c, http.StatusConflict, "PLAN_STATE", "Paket acara tidak dapat dibeli saat ini.", nil)
	case errors.Is(err, ErrIdempotencyMismatch):
		httpapi.Error(c, http.StatusConflict, "IDEMPOTENCY_PAYLOAD_MISMATCH", "Kunci ini sudah dipakai untuk permintaan lain.", nil)
	case errors.Is(err, ErrUnavailable):
		httpapi.Error(c, http.StatusServiceUnavailable, "PAYMENT_UNAVAILABLE", "Pembayaran belum tersedia.", nil)
	case errors.Is(err, ErrProvider):
		httpapi.LogInternalError(c, "payment provider failed", err)
		httpapi.Error(c, http.StatusServiceUnavailable, "PAYMENT_PROVIDER_UNAVAILABLE", "Penyedia pembayaran sedang tidak tersedia. Coba lagi.", nil)
	default:
		httpapi.LogInternalError(c, "plan request failed", err)
		httpapi.Error(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Permintaan paket belum dapat diproses.", nil)
	}
}
