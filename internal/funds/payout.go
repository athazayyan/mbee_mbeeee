package funds

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"kitakarsa.com/tenantku/backend/internal/payment"
	"kitakarsa.com/tenantku/backend/internal/platform/httpapi"
)

// BankAccount is where payouts go. The number is masked when shown back.
type BankAccount struct {
	BankCode      string `json:"bank_code"`
	AccountNumber string `json:"account_number"`
	HolderName    string `json:"holder_name"`
}

var (
	bankCode      = regexp.MustCompile(`^[a-z0-9_]{2,20}$`)
	accountNumber = regexp.MustCompile(`^[0-9]{6,20}$`)
)

// Mask hides all but the last four digits.
func (b BankAccount) Mask() BankAccount {
	if n := len(b.AccountNumber); n > 4 {
		b.AccountNumber = strings.Repeat("•", n-4) + b.AccountNumber[n-4:]
	}
	return b
}

// SetBankAccount saves the organization's payout account. Only the organization's owner may.
func (s Store) SetBankAccount(ctx context.Context, actor, organizationID string, in BankAccount) (BankAccount, error) {
	in.BankCode = strings.ToLower(strings.TrimSpace(in.BankCode))
	in.AccountNumber = strings.NewReplacer(" ", "", "-", "").Replace(in.AccountNumber)
	in.HolderName = strings.Join(strings.Fields(in.HolderName), " ")
	if !httpapi.ValidID(actor) || !httpapi.ValidID(organizationID) {
		return BankAccount{}, ErrNotFound
	}
	if !bankCode.MatchString(in.BankCode) || !accountNumber.MatchString(in.AccountNumber) ||
		utf8.RuneCountInString(in.HolderName) < 1 || utf8.RuneCountInString(in.HolderName) > 100 {
		return BankAccount{}, ErrInvalid
	}
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var owner bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM memberships WHERE user_id = $1 AND organization_id = $2 AND role = 'owner')`,
			actor, organizationID).Scan(&owner); err != nil {
			return err
		}
		if !owner {
			return ErrNotFound
		}
		// The account cannot change while a payout to it is waiting to be confirmed.
		var pending bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM payouts WHERE organization_id = $1 AND status = 'requested')`, organizationID).Scan(&pending); err != nil {
			return err
		}
		if pending {
			return ErrState
		}
		_, err := tx.Exec(ctx, `INSERT INTO organization_bank_accounts (organization_id, bank_code, account_number, holder_name, updated_by)
			VALUES ($1, $2, $3, $4, $5) ON CONFLICT (organization_id) DO UPDATE SET bank_code = EXCLUDED.bank_code,
			account_number = EXCLUDED.account_number, holder_name = EXCLUDED.holder_name,
			updated_by = EXCLUDED.updated_by, updated_at = clock_timestamp()`,
			organizationID, in.BankCode, in.AccountNumber, in.HolderName, actor)
		return err
	})
	return in.Mask(), err
}

// GetBankAccount returns the masked account for the organization's staff.
func (s Store) GetBankAccount(ctx context.Context, actor, organizationID string) (*BankAccount, error) {
	if !httpapi.ValidID(actor) || !httpapi.ValidID(organizationID) {
		return nil, ErrNotFound
	}
	var member bool
	if err := s.DB.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM memberships WHERE user_id = $1 AND organization_id = $2)`, actor, organizationID).Scan(&member); err != nil {
		return nil, err
	}
	if !member {
		return nil, ErrNotFound
	}
	var b BankAccount
	err := s.DB.QueryRow(ctx, `SELECT bank_code, account_number, holder_name FROM organization_bank_accounts WHERE organization_id = $1`, organizationID).
		Scan(&b.BankCode, &b.AccountNumber, &b.HolderName)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	masked := b.Mask()
	return &masked, err
}

// Payout is a request to move released rent to the organizer's bank account.
type Payout struct {
	ID          string     `json:"id"`
	EventID     string     `json:"event_id"`
	Amount      int64      `json:"amount"`
	Status      string     `json:"status"`
	Bank        string     `json:"bank_code"`
	Account     string     `json:"account_number"`
	Holder      string     `json:"holder_name"`
	ProviderRef *string    `json:"provider_ref,omitempty"`
	Failure     *string    `json:"failure,omitempty"`
	RequestedAt time.Time  `json:"requested_at"`
	ReleasedAt  *time.Time `json:"released_at,omitempty"`
}

const payoutColumns = `p.id::text, p.event_id::text, p.amount, p.status, p.bank_code, p.account_number, p.holder_name,
	p.provider_ref, p.failure, p.requested_at, p.released_at`

func scanPayout(row pgx.Row) (Payout, error) {
	var p Payout
	err := row.Scan(&p.ID, &p.EventID, &p.Amount, &p.Status, &p.Bank, &p.Account, &p.Holder, &p.ProviderRef, &p.Failure, &p.RequestedAt, &p.ReleasedAt)
	if err == nil {
		masked := BankAccount{AccountNumber: p.Account}.Mask()
		p.Account = masked.AccountNumber
	}
	return p, err
}

// Blocker codes explain why a payout is not available yet.
const (
	BlockedNotEnded      = "too_early"
	BlockedNoBankAccount = "no_bank_account"
	BlockedOpenRefund    = "open_refund"
	BlockedOpenDispute   = "open_dispute"
	BlockedNoFunds       = "no_funds"
	BlockedAlreadyPaid   = "already_requested"
	BlockedEventStatus   = "event_not_ready"
)

// PayoutState is what the organizer sees on the funds page.
type PayoutState struct {
	EventID       string       `json:"event_id"`
	LastDay       string       `json:"last_day"`
	AvailableFrom string       `json:"available_from"`
	Available     int64        `json:"available"`
	Eligible      bool         `json:"eligible"`
	Blockers      []string     `json:"blockers"`
	BankAccount   *BankAccount `json:"bank_account"`
	Payout        *Payout      `json:"payout"`
}

const dateLayout = "2006-01-02"

// IsWorkingDay is Monday to Friday outside the table of extra non-working days.
func IsWorkingDay(day time.Time, off map[string]bool) bool {
	return day.Weekday() != time.Saturday && day.Weekday() != time.Sunday && !off[day.Format(dateLayout)]
}

// AddWorkingDays moves forward by n working days from a calendar day. Counting starts the day after.
func AddWorkingDays(from time.Time, n int, off map[string]bool) time.Time {
	day := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC)
	for n > 0 {
		day = day.AddDate(0, 0, 1)
		if IsWorkingDay(day, off) {
			n--
		}
	}
	return day
}

// payoutState computes eligibility in the caller's transaction so a request and its check see the same data.
func payoutState(ctx context.Context, tx pgx.Tx, eventID string) (PayoutState, string, error) {
	state := PayoutState{EventID: eventID, Blockers: []string{}}
	var orgID, status, tz string
	var ends, now time.Time
	if err := tx.QueryRow(ctx, `SELECT organization_id::text, status, timezone, ends_at, clock_timestamp() FROM events WHERE id = $1`, eventID).
		Scan(&orgID, &status, &tz, &ends, &now); err != nil {
		return state, "", err
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc = time.UTC
	}
	last := ends.In(loc)
	rows, err := tx.Query(ctx, `SELECT day::text FROM non_working_days`)
	if err != nil {
		return state, "", err
	}
	days, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return state, "", err
	}
	off := map[string]bool{}
	for _, d := range days {
		off[d] = true
	}
	from := AddWorkingDays(last, PayoutWorkingDays, off)
	state.LastDay, state.AvailableFrom = last.Format(dateLayout), from.Format(dateLayout)
	today := now.In(loc).Format(dateLayout)
	if today < state.AvailableFrom {
		state.Blockers = append(state.Blockers, BlockedNotEnded)
	}
	if status != "published" && status != "live" && status != "completed" && status != "cancelled" {
		state.Blockers = append(state.Blockers, BlockedEventStatus)
	}

	// Escrow that belongs to this event: payments in, minus refunds and payouts already set aside.
	if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(CASE e.direction WHEN 'credit' THEN e.amount ELSE -e.amount END), 0)::bigint
		FROM ledger_entries e JOIN ledger_transactions t ON t.id = e.transaction_id
		WHERE e.account = 'organizer_escrow' AND (
			(t.ref_type = 'payment_attempt' AND t.ref_id IN (SELECT a.id FROM payment_attempts a JOIN bookings b ON b.id = a.booking_id WHERE b.event_id = $1))
			OR (t.ref_type IN ('refund', 'refund_fee') AND t.ref_id IN (SELECT id FROM refunds WHERE event_id = $1))
			OR (t.ref_type = 'payout' AND t.ref_id IN (SELECT id FROM payouts WHERE event_id = $1)))`, eventID).Scan(&state.Available); err != nil {
		return state, "", err
	}
	var open bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM refunds WHERE event_id = $1 AND status <> 'refunded')`, eventID).Scan(&open); err != nil {
		return state, "", err
	}
	if open {
		state.Blockers = append(state.Blockers, BlockedOpenRefund)
	}
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM disputes WHERE event_id = $1 AND status = 'open')`, eventID).Scan(&open); err != nil {
		return state, "", err
	}
	if open {
		state.Blockers = append(state.Blockers, BlockedOpenDispute)
	}
	var bank BankAccount
	err = tx.QueryRow(ctx, `SELECT bank_code, account_number, holder_name FROM organization_bank_accounts WHERE organization_id = $1`, orgID).
		Scan(&bank.BankCode, &bank.AccountNumber, &bank.HolderName)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		state.Blockers = append(state.Blockers, BlockedNoBankAccount)
	case err != nil:
		return state, "", err
	default:
		masked := bank.Mask()
		state.BankAccount = &masked
	}
	payout, err := scanPayout(tx.QueryRow(ctx, `SELECT `+payoutColumns+` FROM payouts p WHERE p.event_id = $1
		AND p.status IN ('requested', 'released') LIMIT 1`, eventID))
	if err == nil {
		state.Payout = &payout
		state.Blockers = append(state.Blockers, BlockedAlreadyPaid)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return state, "", err
	}
	if state.Available < 1 && state.Payout == nil {
		state.Blockers = append(state.Blockers, BlockedNoFunds)
	}
	state.Eligible = len(state.Blockers) == 0
	return state, orgID, nil
}

// PayoutStateFor is for the organization's staff. Only the owner can request, but staff can see.
func (s Store) PayoutStateFor(ctx context.Context, actor, eventID string) (PayoutState, error) {
	var state PayoutState
	if !httpapi.ValidID(actor) || !httpapi.ValidID(eventID) {
		return state, ErrNotFound
	}
	err := pgx.BeginTxFunc(ctx, s.DB, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		staff, err := isStaff(ctx, tx, actor, eventID)
		if err != nil {
			return err
		}
		if !staff {
			return ErrNotFound
		}
		state, _, err = payoutState(ctx, tx, eventID)
		return err
	})
	return state, err
}

// BlockedError carries the reasons a payout cannot be requested.
type BlockedError struct{ Blockers []string }

func (BlockedError) Error() string { return ErrPayoutBlocked.Error() }
func (BlockedError) Unwrap() error { return ErrPayoutBlocked }

// RequestPayout is the Cairkan dana button (E09-04). It checks, in one transaction: the caller owns the
// organization, H+4 working days after the event's last day have passed, a bank account exists, no refund
// or dispute is open, and funds remain. It then sets the whole available amount aside from escrow. The same
// key returns the same payout, and a second request for the event is refused.
func (s Store) RequestPayout(ctx context.Context, actor, eventID, key string) (Payout, bool, error) {
	if !httpapi.ValidID(actor) || !httpapi.ValidID(eventID) {
		return Payout{}, false, ErrNotFound
	}
	if n := len(key); n < 1 || n > 128 {
		return Payout{}, false, ErrInvalid
	}
	var result Payout
	var created bool
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		// Serialize requests for one event, whatever the key.
		var locked string
		if err := tx.QueryRow(ctx, `SELECT e.id::text FROM events e JOIN memberships m ON m.organization_id = e.organization_id
			WHERE e.id = $1 AND m.user_id = $2 AND m.role = 'owner' FOR UPDATE OF e`, eventID, actor).Scan(&locked); err != nil {
			return err
		}
		prior, err := scanPayout(tx.QueryRow(ctx, `SELECT `+payoutColumns+` FROM payouts p WHERE p.requested_by = $1 AND p.idempotency_key = $2`, actor, key))
		if err == nil {
			if prior.EventID != eventID {
				return ErrIdempotencyReuse
			}
			result = prior
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		state, orgID, err := payoutState(ctx, tx, eventID)
		if err != nil {
			return err
		}
		if !state.Eligible {
			return BlockedError{Blockers: state.Blockers}
		}
		var bank BankAccount
		if err := tx.QueryRow(ctx, `SELECT bank_code, account_number, holder_name FROM organization_bank_accounts WHERE organization_id = $1`, orgID).
			Scan(&bank.BankCode, &bank.AccountNumber, &bank.HolderName); err != nil {
			return err
		}
		result, err = scanPayout(tx.QueryRow(ctx, `INSERT INTO payouts AS p (event_id, organization_id, amount, status, requested_by,
				bank_code, account_number, holder_name, idempotency_key) VALUES ($1, $2, $3, 'requested', $4, $5, $6, $7, $8)
			RETURNING `+payoutColumns, eventID, orgID, state.Available, actor, bank.BankCode, bank.AccountNumber, bank.HolderName, key))
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23505" {
			return ErrPayoutExists
		}
		if err != nil {
			return err
		}
		created = true
		return payment.ReservePayout(ctx, tx, result.ID, result.Amount)
	})
	return result, created, err
}

// ReleasePayout is the operator's confirmation that the transfer reached the organizer's bank. Only then
// is the payout released and booked (E09-04).
func (s Store) ReleasePayout(ctx context.Context, admin, id, providerRef string) (Payout, error) {
	providerRef = strings.TrimSpace(providerRef)
	if !httpapi.ValidID(id) || providerRef == "" || len(providerRef) > 100 {
		return Payout{}, ErrInvalid
	}
	var result Payout
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if err := requireAdmin(ctx, tx, admin); err != nil {
			return err
		}
		p, err := scanPayout(tx.QueryRow(ctx, `SELECT `+payoutColumns+` FROM payouts p WHERE p.id = $1 FOR UPDATE OF p`, id))
		if err != nil {
			return err
		}
		if p.Status == "released" {
			result = p
			return nil
		}
		if p.Status != "requested" {
			return ErrState
		}
		if err := payment.RecordPayout(ctx, tx, id, p.Amount); err != nil {
			return err
		}
		result, err = scanPayout(tx.QueryRow(ctx, `UPDATE payouts p SET status = 'released', released_at = clock_timestamp(),
			updated_at = clock_timestamp(), provider_ref = $2 WHERE p.id = $1 RETURNING `+payoutColumns, id, providerRef))
		if err != nil {
			return err
		}
		return audit(ctx, tx, admin, "payout.release", "payout", id, providerRef, p, result)
	})
	return result, err
}

// FailPayout records that the transfer did not happen and returns the reserved amount to escrow.
func (s Store) FailPayout(ctx context.Context, admin, id, reason string) (Payout, error) {
	reason = strings.TrimSpace(reason)
	if !httpapi.ValidID(id) || reason == "" || utf8.RuneCountInString(reason) > 300 {
		return Payout{}, ErrInvalid
	}
	var result Payout
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if err := requireAdmin(ctx, tx, admin); err != nil {
			return err
		}
		p, err := scanPayout(tx.QueryRow(ctx, `SELECT `+payoutColumns+` FROM payouts p WHERE p.id = $1 FOR UPDATE OF p`, id))
		if err != nil {
			return err
		}
		if p.Status == "failed" {
			result = p
			return nil
		}
		if p.Status != "requested" {
			return ErrState
		}
		if err := payment.ReleasePayoutReservation(ctx, tx, id); err != nil {
			return err
		}
		result, err = scanPayout(tx.QueryRow(ctx, `UPDATE payouts p SET status = 'failed', failure = $2, updated_at = clock_timestamp()
			WHERE p.id = $1 RETURNING `+payoutColumns, id, reason))
		if err != nil {
			return err
		}
		return audit(ctx, tx, admin, "payout.fail", "payout", id, reason, p, result)
	})
	return result, err
}
