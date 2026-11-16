package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

const (
	purposeEmailVerification = "email_verification"
	purposePasswordReset     = "password_reset"
)

const (
	verificationTTL = 24 * time.Hour
	resetTTL        = 30 * time.Minute
	// Minimum gap between two emails of the same kind for one account.
	resendCooldown = time.Minute
)

var ErrInvalidOrExpiredToken = errors.New("token tidak valid atau sudah kedaluwarsa")

func generateActionToken() (raw string, hash []byte, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, err
	}
	raw = base64.RawURLEncoding.EncodeToString(buf)
	return raw, hashActionToken(raw), nil
}

func hashActionToken(raw string) []byte {
	sum := sha256.Sum256([]byte(raw))
	return sum[:]
}

// createActionToken issues a new token and invalidates older unused ones of the
// same purpose. It reports false (and issues nothing) while the account is still
// inside the resend cooldown, so a caller cannot be used to flood a mailbox.
func (s *Service) createActionToken(ctx context.Context, userID, purpose string, tokenHash []byte, expiresAt time.Time) (bool, error) {
	tx, err := s.store.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)

	// Serialize concurrent requests for the same account.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM users WHERE id = $1::uuid FOR UPDATE`, userID); err != nil {
		return false, err
	}

	var recent bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM auth_action_tokens
			WHERE user_id = $1::uuid AND purpose = $2
			  AND created_at > now() - make_interval(secs => $3)
		)`, userID, purpose, resendCooldown.Seconds()).Scan(&recent); err != nil {
		return false, err
	}
	if recent {
		return false, nil
	}

	if _, err := tx.Exec(ctx, `
		UPDATE auth_action_tokens
		SET used_at = now()
		WHERE user_id = $1::uuid AND purpose = $2 AND used_at IS NULL
	`, userID, purpose); err != nil {
		return false, err
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO auth_action_tokens (token_hash, user_id, purpose, expires_at)
		VALUES ($1, $2::uuid, $3, $4)
	`, tokenHash, userID, purpose, expiresAt); err != nil {
		return false, err
	}

	return true, tx.Commit(ctx)
}

// sendActionEmail issues a token and mails it. Mail failures are logged but not
// returned, so the HTTP response never reveals whether an address is registered.
func (s *Service) sendActionEmail(ctx context.Context, userID, email, purpose string, ttl time.Duration,
	send func(context.Context, string, string) error) error {
	raw, hash, err := generateActionToken()
	if err != nil {
		return err
	}
	issued, err := s.createActionToken(ctx, userID, purpose, hash, time.Now().Add(ttl))
	if err != nil || !issued {
		return err
	}
	if err := send(ctx, email, raw); err != nil {
		log.Printf("auth: send %s email failed: %v", purpose, err)
	}
	return nil
}

func (s *Service) forgotPassword(ctx context.Context, email string) error {
	var userID string
	err := s.store.db.QueryRow(ctx, `SELECT id::text FROM users WHERE email = $1`, email).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}

	return s.sendActionEmail(ctx, userID, email, purposePasswordReset, resetTTL, s.mailer.SendPasswordReset)
}

func (s *Service) resetPassword(ctx context.Context, rawToken, newPassword string) error {
	pwHash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}

	tx, err := s.store.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var userID string
	err = tx.QueryRow(ctx, `
		UPDATE auth_action_tokens
		SET used_at = now()
		WHERE token_hash = $1
		  AND purpose = $2
		  AND used_at IS NULL
		  AND expires_at > now()
		RETURNING user_id::text
	`, hashActionToken(rawToken), purposePasswordReset).Scan(&userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidOrExpiredToken
		}
		return err
	}

	if _, err := tx.Exec(ctx, `
		UPDATE users
		SET password_hash = $2
		WHERE id = $1::uuid
	`, userID, pwHash); err != nil {
		return err
	}

	// Any other outstanding reset link for this account dies with this one.
	if _, err := tx.Exec(ctx, `
		UPDATE auth_action_tokens SET used_at = now()
		WHERE user_id = $1::uuid AND purpose = $2 AND used_at IS NULL
	`, userID, purposePasswordReset); err != nil {
		return err
	}

	// Policy: a reset signs the account out everywhere (all sessions revoked).
	if _, err := tx.Exec(ctx, `
		UPDATE sessions
		SET revoked_at = now()
		WHERE user_id = $1::uuid AND revoked_at IS NULL
	`, userID); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

func (s *Service) verifyEmail(ctx context.Context, rawToken string) error {
	tx, err := s.store.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var userID string
	err = tx.QueryRow(ctx, `
		UPDATE auth_action_tokens
		SET used_at = now()
		WHERE token_hash = $1
		  AND purpose = $2
		  AND used_at IS NULL
		  AND expires_at > now()
		RETURNING user_id::text
	`, hashActionToken(rawToken), purposeEmailVerification).Scan(&userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidOrExpiredToken
		}
		return err
	}

	if _, err := tx.Exec(ctx, `
		UPDATE users
		SET email_verified_at = COALESCE(email_verified_at, now())
		WHERE id = $1::uuid
	`, userID); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

func (s *Service) resendVerification(ctx context.Context, email string) error {
	var userID string
	var verifiedAt *time.Time
	err := s.store.db.QueryRow(ctx, `SELECT id::text, email_verified_at FROM users WHERE email = $1`, email).Scan(&userID, &verifiedAt)
	if errors.Is(err, pgx.ErrNoRows) || verifiedAt != nil {
		return nil
	}
	if err != nil {
		return err
	}

	return s.sendActionEmail(ctx, userID, email, purposeEmailVerification, verificationTTL, s.mailer.SendVerification)
}
