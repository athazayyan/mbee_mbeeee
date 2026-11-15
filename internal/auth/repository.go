package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	errConflict = errors.New("account already exists")
	errSession  = errors.New("invalid session")
)

type Account struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

type repository struct {
	db *pgxpool.Pool
}

func (r repository) createAccount(ctx context.Context, email string, hash []byte) (Account, error) {
	account := Account{Email: email}
	err := r.db.QueryRow(ctx, "INSERT INTO users (email, password_hash) VALUES ($1, $2) RETURNING id", email, hash).Scan(&account.ID)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return Account{}, errConflict
	}
	return account, err
}

func (r repository) credentials(ctx context.Context, email string) (Account, []byte, error) {
	account := Account{Email: email}
	var hash []byte
	err := r.db.QueryRow(ctx, "SELECT id, password_hash FROM users WHERE email = $1", email).Scan(&account.ID, &hash)
	return account, hash, err
}

func (r repository) createSession(ctx context.Context, userID string, expires time.Time, hash []byte) (string, error) {
	var id string
	err := r.db.QueryRow(ctx, `
		WITH created AS (
			INSERT INTO sessions (user_id, expires_at) VALUES ($1, $2) RETURNING id
		)
		INSERT INTO session_refresh_tokens (session_id, token_hash)
		SELECT id, $3 FROM created RETURNING session_id`, userID, expires, hash).Scan(&id)
	return id, err
}

func (r repository) rotate(ctx context.Context, previous, next []byte) (Account, string, time.Time, error) {
	var account Account
	var sessionID string
	var expires time.Time
	err := pgx.BeginFunc(ctx, r.db, func(tx pgx.Tx) error {
		// Lock the session first so concurrent rotations and logout serialize.
		err := tx.QueryRow(ctx, `
			SELECT s.id, u.id, u.email, s.expires_at FROM sessions s
			JOIN users u ON u.id = s.user_id
			JOIN session_refresh_tokens t ON t.session_id = s.id
			WHERE t.token_hash = $1 AND s.revoked_at IS NULL AND s.expires_at > now()
			FOR UPDATE OF s`, previous).Scan(&sessionID, &account.ID, &account.Email, &expires)
		if errors.Is(err, pgx.ErrNoRows) {
			return errSession
		}
		if err != nil {
			return err
		}

		result, err := tx.Exec(ctx, "UPDATE session_refresh_tokens SET used_at = now() WHERE token_hash = $1 AND used_at IS NULL", previous)
		if err != nil {
			return err
		}
		if result.RowsAffected() == 0 {
			// Commit the revocation before returning the authentication error.
			_, err := tx.Exec(ctx, "UPDATE sessions SET revoked_at = now() WHERE id = $1", sessionID)
			sessionID = ""
			return err
		}
		_, err = tx.Exec(ctx, "INSERT INTO session_refresh_tokens (session_id, token_hash) VALUES ($1, $2)", sessionID, next)
		return err
	})
	if err == nil && sessionID == "" {
		err = errSession
	}
	return account, sessionID, expires, err
}

func (r repository) accountForSession(ctx context.Context, userID, sessionID string) (Account, error) {
	var account Account
	err := r.db.QueryRow(ctx, `
		SELECT u.id, u.email FROM users u JOIN sessions s ON s.user_id = u.id
		WHERE u.id = $1 AND s.id = $2 AND s.revoked_at IS NULL AND s.expires_at > now()`, userID, sessionID).
		Scan(&account.ID, &account.Email)
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, errSession
	}
	return account, err
}

func (r repository) revoke(ctx context.Context, hash []byte) error {
	_, err := r.db.Exec(ctx, `
		UPDATE sessions SET revoked_at = now() WHERE revoked_at IS NULL AND id =
		(SELECT session_id FROM session_refresh_tokens WHERE token_hash = $1)`, hash)
	return err
}

func (r repository) check(ctx context.Context) error {
	var tablesPresent bool
	err := r.db.QueryRow(ctx, `
		SELECT to_regclass('users') IS NOT NULL
		AND to_regclass('sessions') IS NOT NULL
		AND to_regclass('session_refresh_tokens') IS NOT NULL`).Scan(&tablesPresent)
	if err != nil {
		return err
	}
	if !tablesPresent {
		return fmt.Errorf("auth migration has not been applied")
	}
	return nil
}
