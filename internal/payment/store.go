package payment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

var (
	ErrNotPayable          = errors.New("booking is not payable")
	ErrIdempotencyMismatch = errors.New("idempotency key reused with a different request")
	ErrUnavailable         = errors.New("payments are not configured")
	// ErrPaymentInProgress means the previous attempt could not be closed, so a new one would risk a double charge.
	ErrPaymentInProgress = errors.New("a payment for this booking is already in progress")
	// ErrQuoteChanged means the total the payer confirmed no longer matches what the server computes.
	ErrQuoteChanged = errors.New("payment total requires confirmation")
)

// legacyMethod keeps the original single-button Snap flow, which charges the booking snapshot total.
const legacyMethod = "snap"

type Store struct {
	DB       *pgxpool.Pool
	Provider *Midtrans
	// PlanRequired reports whether publication needs an active plan; it is informational for clients.
	PlanRequired bool
	// AppOrigin is where Midtrans sends the payer after checkout. Empty leaves the provider default.
	AppOrigin string
	// Simulation lets a payer settle their own open attempt without paying, for the Midtrans sandbox only.
	Simulation bool
}

type Attempt struct {
	ID          string    `json:"id"`
	Status      string    `json:"status"`
	Amount      int64     `json:"amount"`
	Currency    string    `json:"currency"`
	Provider    string    `json:"provider"`
	CheckoutURL string    `json:"checkout_url,omitempty"`
	ExpiresAt   time.Time `json:"expires_at"`
	Method      string    `json:"method"`
	MethodFee   int64     `json:"method_fee"`
}

type Input struct {
	Method      string `json:"method"`
	Description string `json:"description"`
	// ExpectedTotal is the total the payer saw in payment-quotes. It is required for every method but snap.
	ExpectedTotal *int64 `json:"expected_total,omitempty"`
}

func (s Store) Ready(ctx context.Context) error {
	_, err := s.DB.Exec(ctx, `SELECT payment_attempts.rent_amount, payment_attempts.fee_version, payment_attempts.confirmed_type
		FROM payment_attempts, payment_events, payment_exceptions, ledger_transactions, ledger_entries,
		event_plans, event_plan_purchases, payment_methods LIMIT 0`)
	return err
}

const attemptColumns = `id, status, amount, currency, provider, COALESCE(checkout_url, ''), expires_at, method, COALESCE(provider_fee_amount, 0)`

func scanAttempt(row pgx.Row) (Attempt, error) {
	var a Attempt
	err := row.Scan(&a.ID, &a.Status, &a.Amount, &a.Currency, &a.Provider, &a.CheckoutURL, &a.ExpiresAt, &a.Method, &a.MethodFee)
	return a, err
}

// CreateAttempt opens (or returns the existing) Snap payment for a held booking with the chosen method.
// The amount always comes from the booking snapshot plus the method's admin fee; the client never
// supplies it. Choosing a different method closes the live attempt and opens a new one with a fresh fee.
func (s Store) CreateAttempt(ctx context.Context, actor, email, bookingID, key string, input Input) (Attempt, bool, error) {
	if s.Provider == nil {
		return Attempt{}, false, ErrUnavailable
	}
	request := bookingID + "\x00" + input.Method + "\x00" + input.Description
	if input.Method != legacyMethod {
		if input.ExpectedTotal == nil {
			return Attempt{}, false, ErrQuoteChanged
		}
		request += "\x00" + strconv.FormatInt(*input.ExpectedTotal, 10)
	}
	digest := sha256.Sum256([]byte(request))
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
		err := row.Scan(&a.ID, &a.Status, &a.Amount, &a.Currency, &a.Provider, &a.CheckoutURL, &a.ExpiresAt, &a.Method, &a.MethodFee, &prior, &priorBooking)
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
		var total, rent, platformFee, providerFee int64
		var expires, now time.Time
		err = tx.QueryRow(ctx, `SELECT b.status, b.total, b.expires_at, b.rent, b.platform_fee, b.provider_fee
			FROM bookings b JOIN events e ON e.id = b.event_id
			WHERE b.id = $1 AND e.status IN ('published', 'live') AND (b.created_by = $2 OR EXISTS (
				SELECT 1 FROM memberships m WHERE m.user_id = $2 AND m.role = 'owner' AND m.business_id = b.business_id))
			FOR UPDATE OF b`, bookingID, actor).Scan(&status, &total, &expires, &rent, &platformFee, &providerFee)
		if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
			return err
		}
		if (status != "held" && status != "payment_pending") || !expires.After(now) || total < 1 {
			return ErrNotPayable
		}
		// A chosen method replaces the booking's fixed platform and provider fees with its own admin fee.
		// The legacy snap flow still charges the snapshot total.
		var feeVersion *string
		if input.Method != legacyMethod {
			method, err := enabledMethod(ctx, tx, input.Method)
			if err != nil {
				return err
			}
			fee, err := method.Fee(rent)
			if err != nil {
				return err
			}
			total, platformFee, providerFee = rent+fee, 0, fee
			if total < 1 || total > 9007199254740991 || total != *input.ExpectedTotal {
				return ErrQuoteChanged
			}
			feeVersion = &method.FeeVersion
		}
		live, err := scanAttempt(tx.QueryRow(ctx, `SELECT `+attemptColumns+` FROM payment_attempts
			WHERE booking_id = $1 AND status IN ('created', 'pending') FOR UPDATE`, bookingID))
		switch {
		case err == nil && live.Method == input.Method && live.Amount == total:
			result = live
			return nil
		case err == nil:
			// A different method changes the fee and the total, so the old transaction must be closed first.
			outcome, perr := s.Provider.Expire(ctx, live.ID)
			if perr != nil {
				return perr
			}
			if outcome == Refused {
				// Midtrans also refuses when the transaction is already closed, which is safe to replace.
				verified, serr := s.Provider.Status(ctx, live.ID)
				if closed, _ := verified.outcome(); serr != nil || (closed != "failed" && closed != "expired") {
					return ErrPaymentInProgress
				}
			}
			if _, err := tx.Exec(ctx, `UPDATE payment_attempts SET status = 'expired', updated_at = clock_timestamp()
				WHERE id = $1`, live.ID); err != nil {
				return err
			}
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}

		result, err = scanAttempt(tx.QueryRow(ctx, `INSERT INTO payment_attempts
			(booking_id, created_by, provider, method, status, amount, expires_at, idempotency_key, request_digest,
			rent_amount, platform_fee_amount, provider_fee_amount, fee_version)
			VALUES ($1, $2, 'midtrans', $3, 'created', $4, $5, $6, $7, $8, $9, $10, $11) RETURNING `+attemptColumns,
			bookingID, actor, input.Method, total, expires, key, digest[:], rent, platformFee, providerFee, feeVersion))
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

func truncate(value string, limit int) string {
	if runes := []rune(value); len(runes) > limit {
		return string(runes[:limit])
	}
	return value
}

func (s Store) finishURL(path string) string {
	if s.AppOrigin == "" {
		return ""
	}
	return s.AppOrigin + path
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
		var rent, platformFee, providerFee int64
		var remainingSeconds float64
		err = tx.QueryRow(ctx, `SELECT e.name, EXTRACT(EPOCH FROM (b.expires_at - clock_timestamp()))::float8,
			COALESCE(p.rent_amount, b.rent), COALESCE(p.platform_fee_amount, b.platform_fee), COALESCE(p.provider_fee_amount, b.provider_fee)
			FROM bookings b JOIN events e ON e.id = b.event_id JOIN payment_attempts p ON p.booking_id = b.id
			WHERE b.id = $1 AND p.id = $2 AND b.status = 'payment_pending' AND e.status IN ('published', 'live')`, bookingID, id).
			Scan(&eventName, &remainingSeconds, &rent, &platformFee, &providerFee)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotPayable
		}
		if err != nil {
			return err
		}
		if remainingSeconds <= 0 {
			return ErrNotPayable
		}
		if rent+platformFee+providerFee != attempt.Amount {
			return errors.New("payment attempt amount does not match its price split")
		}
		in := SnapInput{
			OrderID: id, Amount: attempt.Amount, Email: email,
			Expiry:    time.Duration(remainingSeconds * float64(time.Second)),
			FinishURL: s.finishURL("/tenant/bookings/" + bookingID + "?pembayaran=selesai"),
		}
		if attempt.Method == legacyMethod {
			in.Items = []snapItem{{ID: bookingID, Price: attempt.Amount, Quantity: 1, Name: truncate("Sewa stand "+eventName, 50)}}
		} else {
			method, err := scanMethod(tx.QueryRow(ctx, `SELECT `+methodColumns+` FROM payment_methods WHERE code = $1`, attempt.Method))
			if err != nil {
				return err
			}
			in.Channels = method.SnapChannels
			for _, item := range []snapItem{
				{ID: "rent", Name: truncate("Sewa stand "+eventName, 50), Price: rent, Quantity: 1},
				{ID: "platform_fee", Name: "Biaya layanan platform", Price: platformFee, Quantity: 1},
				{ID: "payment-admin", Name: truncate("Biaya admin "+method.Label, 50), Price: providerFee, Quantity: 1},
			} {
				if item.Price > 0 {
					in.Items = append(in.Items, item)
				}
			}
		}
		snap, err := s.Provider.CreateSnap(ctx, in)
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

	var attemptID, status, methodCode string
	var bookingRef, purchaseRef *string
	var amount int64
	err := pgx.ErrNoRows
	if httpapi.ValidID(n.OrderID) {
		err = tx.QueryRow(ctx, `SELECT id, booking_id, purchase_id, status, amount, method
			FROM payment_attempts WHERE id = $1 FOR UPDATE`,
			n.OrderID).Scan(&attemptID, &bookingRef, &purchaseRef, &status, &amount, &methodCode)
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
	if n.PaymentType != "" {
		// Bind the method the payer actually used to the attempt (E08-02).
		if _, err := tx.Exec(ctx, `UPDATE payment_attempts SET confirmed_type = $2, confirmed_detail = NULLIF($3, '')
			WHERE id = $1`, attemptID, n.PaymentType, n.Detail()); err != nil {
			return "", err
		}
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
		var replaced bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM payment_attempts
			WHERE booking_id = $1 AND status IN ('created', 'pending'))`, bookingID).Scan(&replaced); err != nil {
			return "", err
		}
		// A newer attempt (for example after switching method) keeps the booking in payment_pending.
		if bookingStatus == "payment_pending" && !replaced {
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
	// The attempt's own breakdown is what was charged; the booking snapshot only covers older attempts.
	if err := tx.QueryRow(ctx, `SELECT b.status, COALESCE(p.rent_amount, b.rent),
		COALESCE(p.platform_fee_amount, b.platform_fee), COALESCE(p.provider_fee_amount, b.provider_fee), b.expires_at
		FROM bookings b JOIN payment_attempts p ON p.booking_id = b.id AND p.id = $2
		WHERE b.id = $1 FOR UPDATE OF b`, bookingID, attemptID).Scan(&bookingStatus, &rent, &platformFee, &providerFee, &expires); err != nil {
		return "", err
	}
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return "", err
	}
	// The payer must have used the method the fee was priced for; a different one is reviewed by hand.
	if n.PaymentType != "" {
		if method, err := scanMethod(tx.QueryRow(ctx, `SELECT `+methodColumns+` FROM payment_methods WHERE code = $1`, methodCode)); err == nil {
			if !method.Confirms(n.PaymentType) {
				if err := exception("method_mismatch", &attemptID, &bookingID,
					map[string]any{"chosen": methodCode, "paid_with": n.PaymentType}); err != nil {
					return "", err
				}
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return "", err
		}
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
	// Any other open attempt for this booking can no longer be paid validly; a late payment on it becomes a duplicate.
	if _, err := tx.Exec(ctx, `UPDATE payment_attempts SET status = 'expired', updated_at = clock_timestamp()
		WHERE booking_id = $1 AND id <> $2 AND status IN ('created', 'pending')`, bookingID, attemptID); err != nil {
		return "", err
	}
	if rent+platformFee+providerFee != amount {
		return "applied", exception("ledger_mismatch", &attemptID, &bookingID,
			map[string]any{"amount": amount, "rent": rent, "platform_fee": platformFee, "provider_fee": providerFee})
	}
	return "applied", recordPayment(ctx, tx, attemptID, rent, platformFee, providerFee)
}

type exceptionFn func(kind string, attempt, booking *string, detail map[string]any) error
