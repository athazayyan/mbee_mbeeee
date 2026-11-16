package payment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

var (
	ErrNotPayable          = errors.New("booking is not payable")
	ErrIdempotencyMismatch = errors.New("idempotency key reused with a different request")
	ErrUnavailable         = errors.New("payments are not configured")
)

type Store struct {
	DB       *pgxpool.Pool
	Provider *Midtrans
	// PlanRequired reports whether publication needs an active plan; it is informational for clients.
	PlanRequired bool
}

type Attempt struct {
	ID          string    `json:"id"`
	Status      string    `json:"status"`
	Amount      int64     `json:"amount"`
	Currency    string    `json:"currency"`
	Provider    string    `json:"provider"`
	CheckoutURL string    `json:"checkout_url,omitempty"`
	ExpiresAt   time.Time `json:"expires_at"`
}

type Input struct {
	Method      string `json:"method"`
	Description string `json:"description"`
}

func (s Store) Ready(ctx context.Context) error {
	_, err := s.DB.Exec(ctx, `SELECT 1 FROM payment_attempts, payment_events, payment_exceptions,
		ledger_transactions, ledger_entries, event_plans, event_plan_purchases LIMIT 0`)
	return err
}

const attemptColumns = `id, status, amount, currency, provider, COALESCE(checkout_url, ''), expires_at`

func scanAttempt(row pgx.Row) (Attempt, error) {
	var a Attempt
	err := row.Scan(&a.ID, &a.Status, &a.Amount, &a.Currency, &a.Provider, &a.CheckoutURL, &a.ExpiresAt)
	return a, err
}

// CreateAttempt opens (or returns the existing) Snap payment for a held booking. The amount always
// comes from the booking's price snapshot; the client never supplies it.
func (s Store) CreateAttempt(ctx context.Context, actor, email, bookingID, key string, input Input) (Attempt, bool, error) {
	if s.Provider == nil {
		return Attempt{}, false, ErrUnavailable
	}
	digest := sha256.Sum256([]byte(bookingID + "\x00" + input.Method + "\x00" + input.Description))
	var result Attempt
	var created bool
	err := pgx.BeginTxFunc(ctx, s.DB, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(
			hashtextextended('tenantku.payment.attempt:' || $1::uuid::text, 0))`, actor); err != nil {
			return err
		}
		var prior []byte
		var priorBooking *string
		row := tx.QueryRow(ctx, `SELECT `+attemptColumns+`, request_digest, booking_id FROM payment_attempts
			WHERE created_by = $1 AND idempotency_key = $2`, actor, key)
		var a Attempt
		err := row.Scan(&a.ID, &a.Status, &a.Amount, &a.Currency, &a.Provider, &a.CheckoutURL, &a.ExpiresAt, &prior, &priorBooking)
		if err == nil {
			if !bytes.Equal(prior, digest[:]) || priorBooking == nil || *priorBooking != bookingID {
				return ErrIdempotencyMismatch
			}
			result = a
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		var status string
		var total int64
		var expires, now time.Time
		err = tx.QueryRow(ctx, `SELECT b.status, b.total, b.expires_at
			FROM bookings b JOIN events e ON e.id = b.event_id
			WHERE b.id = $1 AND e.status IN ('published', 'live') AND (b.created_by = $2 OR EXISTS (
				SELECT 1 FROM memberships m WHERE m.user_id = $2 AND m.role = 'owner' AND m.business_id = b.business_id))
			FOR UPDATE OF b`, bookingID, actor).Scan(&status, &total, &expires)
		if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
			return err
		}
		if (status != "held" && status != "payment_pending") || !expires.After(now) || total < 1 {
			return ErrNotPayable
		}
		live, err := scanAttempt(tx.QueryRow(ctx, `SELECT `+attemptColumns+` FROM payment_attempts
			WHERE booking_id = $1 AND status IN ('created', 'pending')`, bookingID))
		if err == nil {
			result = live
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		result, err = scanAttempt(tx.QueryRow(ctx, `INSERT INTO payment_attempts
			(booking_id, created_by, provider, method, status, amount, expires_at, idempotency_key, request_digest)
			VALUES ($1, $2, 'midtrans', $3, 'created', $4, $5, $6, $7) RETURNING `+attemptColumns,
			bookingID, actor, input.Method, total, expires, key, digest[:]))
		if err != nil {
			return err
		}
		if status == "held" {
			if err := setBookingStatus(ctx, tx, bookingID, status, "payment_pending", "payment attempt reserved"); err != nil {
				return err
			}
		}
		created = true
		return nil
	})
	if err != nil {
		return Attempt{}, false, err
	}
	if result.Status == "created" {
		// The attempt and its order ID survive a lost provider response.
		result, err = s.openAttempt(ctx, result.ID, bookingID, email)
	}
	return result, created && err == nil, err
}

func (s Store) openAttempt(ctx context.Context, id, bookingID, email string) (Attempt, error) {
	var result Attempt
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		attempt, err := scanAttempt(tx.QueryRow(ctx, `SELECT `+attemptColumns+` FROM payment_attempts WHERE id = $1 AND booking_id = $2 FOR UPDATE`, id, bookingID))
		if err != nil {
			return err
		}
		if attempt.Status != "created" {
			result = attempt
			return nil
		}
		var eventName string
		var remainingSeconds float64
		err = tx.QueryRow(ctx, `SELECT e.name, EXTRACT(EPOCH FROM (b.expires_at - clock_timestamp()))::float8
			FROM bookings b JOIN events e ON e.id = b.event_id
			WHERE b.id = $1 AND b.status = 'payment_pending' AND e.status IN ('published', 'live')`, bookingID).
			Scan(&eventName, &remainingSeconds)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotPayable
		}
		if err != nil {
			return err
		}
		if remainingSeconds <= 0 {
			return ErrNotPayable
		}
		name := eventName
		if len([]rune(name)) > 50 {
			name = string([]rune(name)[:50])
		}
		snap, err := s.Provider.CreateSnap(ctx, id, attempt.Amount, email,
			[]snapItem{{ID: bookingID, Price: attempt.Amount, Quantity: 1, Name: "Sewa stand " + name}},
			time.Duration(remainingSeconds*float64(time.Second)))
		if err != nil {
			return err
		}
		var payable bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM bookings b JOIN events e ON e.id = b.event_id
			WHERE b.id = $1 AND b.status = 'payment_pending' AND b.expires_at > clock_timestamp()
				AND e.status IN ('published', 'live')
		)`, bookingID).Scan(&payable); err != nil {
			return err
		}
		if !payable {
			return ErrNotPayable
		}
		result, err = scanAttempt(tx.QueryRow(ctx, `UPDATE payment_attempts SET status = 'pending', provider_token = $2,
			checkout_url = $3, updated_at = clock_timestamp() WHERE id = $1 RETURNING `+attemptColumns, id, snap.Token, snap.RedirectURL))
		return err
	})
	return result, err
}

func setBookingStatus(ctx context.Context, tx pgx.Tx, bookingID, from, to, reason string) error {
	if _, err := tx.Exec(ctx, `UPDATE bookings SET status = $2 WHERE id = $1`, bookingID, to); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO booking_status_history (booking_id, from_status, to_status, reason)
		VALUES ($1, $2, $3, $4)`, bookingID, from, to, reason)
	return err
}

// ApplyNotification stores a verified provider event and applies at most one state change. It is
// safe to call repeatedly with the same event.
func (s Store) ApplyNotification(ctx context.Context, n Notification, raw []byte) error {
	dedupe := n.OrderID + "|" + n.TransactionStatus + "|" + n.StatusCode + "|" + n.FraudStatus + "|" + n.TransactionID + "|" + n.GrossAmount
	return pgx.BeginTxFunc(ctx, s.DB, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var eventID int64
		err := tx.QueryRow(ctx, `INSERT INTO payment_events (provider, dedupe_key, provider_status, outcome, payload)
			VALUES ('midtrans', $1, $2, 'ignored', $3) ON CONFLICT (provider, dedupe_key) DO NOTHING RETURNING id`,
			dedupe, n.TransactionStatus, json.RawMessage(raw)).Scan(&eventID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // already processed
		}
		if err != nil {
			return err
		}
		outcome, err := s.apply(ctx, tx, n, eventID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE payment_events SET outcome = $2 WHERE id = $1`, eventID, outcome)
		return err
	})
}

func (s Store) apply(ctx context.Context, tx pgx.Tx, n Notification, eventID int64) (string, error) {
	exception := exceptionFn(func(kind string, attempt, booking *string, detail map[string]any) error {
		detail["order_id"], detail["provider_status"] = n.OrderID, n.TransactionStatus
		raw, _ := json.Marshal(detail)
		_, err := tx.Exec(ctx, `INSERT INTO payment_exceptions (attempt_id, booking_id, kind, detail) VALUES ($1, $2, $3, $4)`,
			attempt, booking, kind, raw)
		return err
	})

	var attemptID, status string
	var bookingRef, purchaseRef *string
	var amount int64
	err := pgx.ErrNoRows
	if httpapi.ValidID(n.OrderID) {
		err = tx.QueryRow(ctx, `SELECT id, booking_id, purchase_id, status, amount FROM payment_attempts WHERE id = $1 FOR UPDATE`,
			n.OrderID).Scan(&attemptID, &bookingRef, &purchaseRef, &status, &amount)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return "ignored", exception("unknown_order", nil, nil, map[string]any{})
	}
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `UPDATE payment_events SET attempt_id = $2 WHERE id = $1`, eventID, attemptID); err != nil {
		return "", err
	}

	gross, ok := wholeRupiah(n.GrossAmount)
	if !ok || gross != amount || (n.Currency != "" && n.Currency != "IDR") {
		return "rejected", exception("amount_mismatch", &attemptID, bookingRef,
			map[string]any{"expected": amount, "got": n.GrossAmount, "currency": n.Currency})
	}
	next, ok := n.outcome()
	if !ok || status == "paid" {
		return "ignored", nil
	}
	if purchaseRef != nil {
		return s.applyPlan(ctx, tx, attemptID, *purchaseRef, status, amount, next, exception)
	}
	bookingID := *bookingRef

	switch next {
	case "pending":
		if status == "created" {
			_, err = tx.Exec(ctx, `UPDATE payment_attempts SET status = 'pending', updated_at = clock_timestamp() WHERE id = $1`, attemptID)
		}
		return "applied", err
	case "failed", "expired":
		if status != "created" && status != "pending" {
			return "ignored", nil
		}
		if _, err := tx.Exec(ctx, `UPDATE payment_attempts SET status = $2, updated_at = clock_timestamp() WHERE id = $1`, attemptID, next); err != nil {
			return "", err
		}
		var bookingStatus string
		if err := tx.QueryRow(ctx, `SELECT status FROM bookings WHERE id = $1 FOR UPDATE`, bookingID).Scan(&bookingStatus); err != nil {
			return "", err
		}
		if bookingStatus == "payment_pending" {
			err = setBookingStatus(ctx, tx, bookingID, bookingStatus, "held", "payment attempt "+next)
		}
		return "applied", err
	}

	// paid: money was received for this attempt, even if the attempt had already been closed locally.
	if _, err := tx.Exec(ctx, `UPDATE payment_attempts SET status = 'paid', paid_at = clock_timestamp(),
		updated_at = clock_timestamp() WHERE id = $1`, attemptID); err != nil {
		return "", err
	}
	var bookingStatus string
	var rent, platformFee, providerFee int64
	var expires, now time.Time
	if err := tx.QueryRow(ctx, `SELECT status, rent, platform_fee, provider_fee, expires_at
		FROM bookings WHERE id = $1 FOR UPDATE`, bookingID).Scan(&bookingStatus, &rent, &platformFee, &providerFee, &expires); err != nil {
		return "", err
	}
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return "", err
	}
	switch {
	case bookingStatus == "confirmed":
		return "applied", exception("duplicate_payment", &attemptID, &bookingID, map[string]any{"amount": amount})
	case (bookingStatus != "held" && bookingStatus != "payment_pending") || !expires.After(now):
		return "applied", exception("late_payment", &attemptID, &bookingID,
			map[string]any{"amount": amount, "booking_status": bookingStatus})
	}
	if err := setBookingStatus(ctx, tx, bookingID, bookingStatus, "confirmed", "payment settled"); err != nil {
		return "", err
	}
	tag, err := tx.Exec(ctx, `UPDATE allocations SET status = 'reserved' WHERE booking_id = $1 AND status IN ('hold', 'offer')`, bookingID)
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() != 1 {
		return "", errors.New("payment confirmation requires a held allocation")
	}
	return "applied", recordPayment(ctx, tx, attemptID, amount, rent, platformFee, providerFee)
}

// recordPayment writes one balanced, append-only ledger transaction for a settled attempt.
func recordPayment(ctx context.Context, tx pgx.Tx, attemptID string, total, rent, platformFee, providerFee int64) error {
	var txID string
	err := tx.QueryRow(ctx, `INSERT INTO ledger_transactions (kind, ref_type, ref_id)
		VALUES ('payment_received', 'payment_attempt', $1) ON CONFLICT DO NOTHING RETURNING id`, attemptID).Scan(&txID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO ledger_entries (transaction_id, account, direction, amount)
		SELECT $1, account, direction, amount FROM (VALUES
			('provider_clearing', 'debit', $2::bigint), ('organizer_escrow', 'credit', $3::bigint),
			('platform_revenue', 'credit', $4::bigint), ('provider_fee_payable', 'credit', $5::bigint)
		) AS v(account, direction, amount) WHERE amount > 0`, txID, total, rent, platformFee, providerFee)
	return err
}

type exceptionFn func(kind string, attempt, booking *string, detail map[string]any) error
