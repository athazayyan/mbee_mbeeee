package auth

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
)

type captureMailer struct {
	mu     sync.Mutex
	tokens map[string][]string // "kind:email" -> raw tokens
}

func (m *captureMailer) add(kind, email, token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tokens == nil {
		m.tokens = map[string][]string{}
	}
	m.tokens[kind+":"+email] = append(m.tokens[kind+":"+email], token)
	return nil
}

func (m *captureMailer) SendVerification(_ context.Context, email, token string) error {
	return m.add("verify", email, token)
}

func (m *captureMailer) SendPasswordReset(_ context.Context, email, token string) error {
	return m.add("reset", email, token)
}

func (m *captureMailer) last(t *testing.T, kind, email string) string {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	list := m.tokens[kind+":"+email]
	if len(list) == 0 {
		t.Fatalf("no %s email sent to %s", kind, email)
	}
	return list[len(list)-1]
}

func (m *captureMailer) count(kind, email string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.tokens[kind+":"+email])
}

func newTokenFixture(t *testing.T) (*Service, *pgxpool.Pool, *captureMailer, context.Context) {
	t.Helper()
	databaseURL := os.Getenv("TENANTKU_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set TENANTKU_TEST_DATABASE_URL to test against a migrated PostgreSQL database")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	service, err := New(ctx, pool, rand.Text()+rand.Text(), rand.Text()+rand.Text())
	if err != nil {
		t.Fatal(err)
	}
	mailer := &captureMailer{}
	service.mailer = mailer
	return service, pool, mailer, ctx
}

func registerFixture(t *testing.T, s *Service, pool *pgxpool.Pool, ctx context.Context, password string) Account {
	t.Helper()
	email := strings.ToLower(rand.Text()) + "@example.com"
	account, err := s.register(ctx, email, password)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), "DELETE FROM users WHERE id = $1", account.ID); err != nil {
			t.Error(err)
		}
	})
	return account
}

// Make the cooldown window pass without sleeping.
func expireCooldown(t *testing.T, pool *pgxpool.Pool, ctx context.Context, userID string) {
	t.Helper()
	if _, err := pool.Exec(ctx, "UPDATE auth_action_tokens SET created_at = now() - interval '2 minutes' WHERE user_id = $1", userID); err != nil {
		t.Fatal(err)
	}
}

func TestEmailVerification(t *testing.T) {
	s, pool, mailer, ctx := newTokenFixture(t)
	account := registerFixture(t, s, pool, ctx, "original password 1")

	token := mailer.last(t, "verify", account.Email)
	if err := s.verifyEmail(ctx, token); err != nil {
		t.Fatalf("verify: %v", err)
	}
	var verified bool
	if err := pool.QueryRow(ctx, "SELECT email_verified_at IS NOT NULL FROM users WHERE id = $1", account.ID).Scan(&verified); err != nil || !verified {
		t.Fatalf("email not marked verified: %v %v", verified, err)
	}
	if err := s.verifyEmail(ctx, token); !errors.Is(err, ErrInvalidOrExpiredToken) {
		t.Fatalf("reused token: got %v", err)
	}
	if err := s.verifyEmail(ctx, "not-a-real-token"); !errors.Is(err, ErrInvalidOrExpiredToken) {
		t.Fatalf("unknown token: got %v", err)
	}

	// Already verified: resend sends nothing, and does not error.
	before := mailer.count("verify", account.Email)
	expireCooldown(t, pool, ctx, account.ID)
	if err := s.resendVerification(ctx, account.Email); err != nil {
		t.Fatal(err)
	}
	if mailer.count("verify", account.Email) != before {
		t.Fatal("resend emailed an already verified account")
	}
}

func TestResendReplacesTokenAndHonoursCooldown(t *testing.T) {
	s, pool, mailer, ctx := newTokenFixture(t)
	account := registerFixture(t, s, pool, ctx, "original password 1")
	first := mailer.last(t, "verify", account.Email)

	// Register just issued a token: an immediate resend is swallowed.
	if err := s.resendVerification(ctx, account.Email); err != nil {
		t.Fatal(err)
	}
	if n := mailer.count("verify", account.Email); n != 1 {
		t.Fatalf("cooldown ignored: %d emails", n)
	}

	expireCooldown(t, pool, ctx, account.ID)
	if err := s.resendVerification(ctx, account.Email); err != nil {
		t.Fatal(err)
	}
	second := mailer.last(t, "verify", account.Email)
	if first == second {
		t.Fatal("resend reused the old token")
	}
	if err := s.verifyEmail(ctx, first); !errors.Is(err, ErrInvalidOrExpiredToken) {
		t.Fatalf("superseded token still works: %v", err)
	}
	if err := s.verifyEmail(ctx, second); err != nil {
		t.Fatalf("new token rejected: %v", err)
	}
}

func TestExpiredTokenRejected(t *testing.T) {
	s, pool, mailer, ctx := newTokenFixture(t)
	account := registerFixture(t, s, pool, ctx, "original password 1")
	token := mailer.last(t, "verify", account.Email)
	if _, err := pool.Exec(ctx, "UPDATE auth_action_tokens SET expires_at = now() - interval '1 second' WHERE user_id = $1", account.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.verifyEmail(ctx, token); !errors.Is(err, ErrInvalidOrExpiredToken) {
		t.Fatalf("expired token: got %v", err)
	}
}

func TestPasswordResetRevokesSessionsAndIsSingleUse(t *testing.T) {
	s, pool, mailer, ctx := newTokenFixture(t)
	const oldPassword, newPassword = "original password 1", "brand new password 2"
	account := registerFixture(t, s, pool, ctx, oldPassword)

	_, session, err := s.login(ctx, account.Email, oldPassword)
	if err != nil {
		t.Fatal(err)
	}

	// Unknown address: no error, no email.
	if err := s.forgotPassword(ctx, "nobody-"+account.Email); err != nil {
		t.Fatalf("unknown email must not error: %v", err)
	}
	if err := s.forgotPassword(ctx, account.Email); err != nil {
		t.Fatal(err)
	}
	token := mailer.last(t, "reset", account.Email)

	if err := s.resetPassword(ctx, token, newPassword); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if err := s.resetPassword(ctx, token, "another password 3"); !errors.Is(err, ErrInvalidOrExpiredToken) {
		t.Fatalf("reused reset token: got %v", err)
	}
	if _, _, err := s.login(ctx, account.Email, oldPassword); !errors.Is(err, errSession) {
		t.Fatalf("old password still works: %v", err)
	}
	if _, _, err := s.login(ctx, account.Email, newPassword); err != nil {
		t.Fatalf("new password rejected: %v", err)
	}
	if _, err := s.account(ctx, session.access); !errors.Is(err, errSession) {
		t.Fatalf("pre-reset access token still valid: %v", err)
	}
	if _, _, err := s.refresh(ctx, session.refresh); !errors.Is(err, errSession) {
		t.Fatalf("pre-reset refresh token still valid: %v", err)
	}
}

func TestNewResetLinkSupersedesOldOne(t *testing.T) {
	s, pool, mailer, ctx := newTokenFixture(t)
	account := registerFixture(t, s, pool, ctx, "original password 1")
	if err := s.forgotPassword(ctx, account.Email); err != nil {
		t.Fatal(err)
	}
	first := mailer.last(t, "reset", account.Email)
	expireCooldown(t, pool, ctx, account.ID)
	if err := s.forgotPassword(ctx, account.Email); err != nil {
		t.Fatal(err)
	}
	if err := s.resetPassword(ctx, first, "brand new password 2"); !errors.Is(err, ErrInvalidOrExpiredToken) {
		t.Fatalf("superseded reset token still works: %v", err)
	}
}

func TestResetTokenCannotVerifyEmail(t *testing.T) {
	s, pool, mailer, ctx := newTokenFixture(t)
	account := registerFixture(t, s, pool, ctx, "original password 1")
	if err := s.forgotPassword(ctx, account.Email); err != nil {
		t.Fatal(err)
	}
	if err := s.verifyEmail(ctx, mailer.last(t, "reset", account.Email)); !errors.Is(err, ErrInvalidOrExpiredToken) {
		t.Fatalf("token purposes are not separated: %v", err)
	}
}

func TestLoginRequiresVerifiedEmail(t *testing.T) {
	s, pool, mailer, ctx := newTokenFixture(t)
	const password = "verified login password"
	account := registerFixture(t, s, pool, ctx, password)

	// Off by default, so existing accounts and fixtures keep working.
	if _, _, err := s.login(ctx, account.Email, password); err != nil {
		t.Fatalf("login with the requirement off: %v", err)
	}

	s.RequireVerifiedEmail(true)
	if _, _, err := s.login(ctx, account.Email, password); !errors.Is(err, ErrEmailNotVerified) {
		t.Fatalf("unverified login: got %v", err)
	}
	// A wrong password or unknown address must look the same as before, so nothing leaks about registration.
	if _, _, err := s.login(ctx, account.Email, "wrong password entirely"); !errors.Is(err, errSession) {
		t.Fatalf("wrong password on unverified account: got %v", err)
	}
	if _, _, err := s.login(ctx, "nobody-"+account.Email, password); !errors.Is(err, errSession) {
		t.Fatalf("unknown email: got %v", err)
	}
	var sessions int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM sessions WHERE user_id = $1", account.ID).Scan(&sessions); err != nil || sessions != 1 {
		t.Fatalf("refused login must not create a session: count=%d err=%v", sessions, err)
	}

	if err := s.verifyEmail(ctx, mailer.last(t, "verify", account.Email)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.login(ctx, account.Email, password); err != nil {
		t.Fatalf("login after verification: %v", err)
	}
}

func TestEmailNotVerifiedResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/auth/login", nil)
	sendError(c, ErrEmailNotVerified)
	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), `"code":"EMAIL_NOT_VERIFIED"`) {
		t.Fatalf("got %d %s", recorder.Code, recorder.Body.String())
	}
}
