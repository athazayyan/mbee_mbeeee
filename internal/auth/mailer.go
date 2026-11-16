package auth

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"strings"
)

type Mailer interface {
	SendVerification(ctx context.Context, toEmail, rawToken string) error

	SendPasswordReset(ctx context.Context, toEmail, rawToken string) error
}

type LogMailer struct {
	AppOrigin string
}

func NewLogMailer(appOrigin string) *LogMailer {
	return &LogMailer{AppOrigin: strings.TrimSuffix(appOrigin, "/")}
}

func (m *LogMailer) SendVerification(_ context.Context, toEmail, rawToken string) error {
	link := fmt.Sprintf("%s/verify-email?token=%s", m.AppOrigin, url.QueryEscape(rawToken))
	log.Printf("Email to %s \nVerification URL:\n%s\n", toEmail, link)
	return nil
}

func (m *LogMailer) SendPasswordReset(_ context.Context, toEmail, rawToken string) error {
	link := fmt.Sprintf("%s/reset-password?token=%s", m.AppOrigin, url.QueryEscape(rawToken))
	log.Printf("Email to %s \nPassword Reset URL:\n%s\n", toEmail, link)
	return nil
}
