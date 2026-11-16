package auth

import "context"

type Mailer interface {
	SendVerification(ctx context.Context, toEmail, rawToken string) error
	SendPasswordReset(ctx context.Context, toEmail, rawToken string) error
}
