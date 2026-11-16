package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"log"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

const (
	tokenIssuer   = "tenantku"
	tokenAudience = "tenantku-app"
	accessTTL     = 10 * time.Minute
	sessionTTL    = 30 * 24 * time.Hour
)

type Service struct {
	store     repository
	key       []byte
	pepper    []byte
	dummyHash []byte
	mailer    Mailer
	mailerSet bool
}

func New(ctx context.Context, db *pgxpool.Pool, key, pepper string) (*Service, error) {
	if len(key) < 32 || len(pepper) < 32 || key == pepper {
		return nil, errors.New("auth requires distinct signing and refresh secrets of at least 32 bytes")
	}
	store := repository{db: db}
	if err := store.check(ctx); err != nil {
		return nil, err
	}
	dummyHash, err := bcrypt.GenerateFromPassword([]byte(rand.Text()), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	return &Service{store: store, key: []byte(key), pepper: []byte(pepper), dummyHash: dummyHash, mailer: NewLogMailer("http://localhost:3001")}, nil
}

// UseMailer replaces the default log-only mailer. Call before Routes.
func (s *Service) UseMailer(m Mailer) {
	s.mailer, s.mailerSet = m, true
}

func (s *Service) Ready(ctx context.Context) error {
	return s.store.check(ctx)
}

type tokens struct {
	access  string
	refresh string
	expires time.Time
}

type claims struct {
	SessionID string `json:"sid"`
	jwt.RegisteredClaims
}

func (s *Service) issue(account Account, id, refresh string, expires time.Time) (tokens, error) {
	now := time.Now()
	accessExpires := now.Add(accessTTL)
	if expires.Before(accessExpires) {
		accessExpires = expires
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims{
		SessionID: id,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: tokenIssuer, Subject: account.ID, Audience: jwt.ClaimStrings{tokenAudience},
			IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(accessExpires),
		},
	})
	access, err := token.SignedString(s.key)
	return tokens{access: access, refresh: refresh, expires: expires}, err
}

func refreshToken() string {
	var token [32]byte
	rand.Read(token[:])
	return base64.RawURLEncoding.EncodeToString(token[:])
}

func (s *Service) hashRefresh(token string) []byte {
	mac := hmac.New(sha256.New, s.pepper)
	mac.Write([]byte(token))
	return mac.Sum(nil)
}

func (s *Service) register(ctx context.Context, email, password string) (Account, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return Account{}, err
	}
	account, err := s.store.createAccount(ctx, email, hash)
	if err != nil {
		return Account{}, err
	}
	// Best effort: the account exists even if the first email cannot be issued;
	// the user can request another one via resend-verification.
	if err := s.sendActionEmail(ctx, account.ID, email, purposeEmailVerification, verificationTTL, s.mailer.SendVerification); err != nil {
		log.Printf("auth: could not issue verification token: %v", err)
	}
	return account, nil
}

func (s *Service) login(ctx context.Context, email, password string) (Account, tokens, error) {
	account, hash, err := s.store.credentials(ctx, email)
	if errors.Is(err, pgx.ErrNoRows) {
		hash = s.dummyHash
	} else if err != nil {
		return Account{}, tokens{}, err
	}
	if bcrypt.CompareHashAndPassword(hash, []byte(password)) != nil || account.ID == "" {
		return Account{}, tokens{}, errSession
	}
	refresh := refreshToken()
	expires := time.Now().Add(sessionTTL)
	id, err := s.store.createSession(ctx, account.ID, expires, s.hashRefresh(refresh))
	if err != nil {
		return Account{}, tokens{}, err
	}
	result, err := s.issue(account, id, refresh, expires)
	return account, result, err
}

func (s *Service) refresh(ctx context.Context, previous string) (Account, tokens, error) {
	next := refreshToken()
	account, id, expires, err := s.store.rotate(ctx, s.hashRefresh(previous), s.hashRefresh(next))
	if err != nil {
		return Account{}, tokens{}, err
	}
	result, err := s.issue(account, id, next, expires)
	return account, result, err
}

func (s *Service) account(ctx context.Context, access string) (Account, error) {
	var payload claims
	token, err := jwt.ParseWithClaims(access, &payload, func(*jwt.Token) (any, error) {
		return s.key, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(tokenIssuer), jwt.WithAudience(tokenAudience), jwt.WithExpirationRequired())
	if err != nil || !token.Valid || payload.Subject == "" || payload.SessionID == "" {
		return Account{}, errSession
	}
	return s.store.accountForSession(ctx, payload.Subject, payload.SessionID)
}

func (s *Service) logout(ctx context.Context, refresh string) error {
	return s.store.revoke(ctx, s.hashRefresh(refresh))
}
