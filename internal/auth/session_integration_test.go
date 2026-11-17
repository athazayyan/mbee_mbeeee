package auth

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRefreshReplayRevokesSession(t *testing.T) {
	databaseURL := os.Getenv("TENANTKU_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set TENANTKU_TEST_DATABASE_URL to test against a migrated PostgreSQL database")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	service, err := New(ctx, pool, rand.Text()+rand.Text(), rand.Text()+rand.Text())
	if err != nil {
		t.Fatal(err)
	}
	email := strings.ToLower(rand.Text()) + "@example.com"
	const password = "session fixture password"
	account, err := service.register(ctx, email, password, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if _, err := pool.Exec(cleanup, "DELETE FROM users WHERE id = $1", account.ID); err != nil {
			t.Error(err)
		}
	})

	_, previous, err := service.login(ctx, email, password)
	if err != nil {
		t.Fatal(err)
	}
	_, current, err := service.refresh(ctx, previous.refresh)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.account(ctx, current.access); err != nil {
		t.Fatalf("rotated session was rejected: %v", err)
	}
	if _, _, err := service.refresh(ctx, previous.refresh); !errors.Is(err, errSession) {
		t.Fatalf("replayed refresh token: got %v, want invalid session", err)
	}
	if _, err := service.account(ctx, current.access); !errors.Is(err, errSession) {
		t.Fatalf("access token after replay: got %v, want invalid session", err)
	}
	if _, _, err := service.refresh(ctx, current.refresh); !errors.Is(err, errSession) {
		t.Fatalf("rotated refresh token after replay: got %v, want invalid session", err)
	}
}
