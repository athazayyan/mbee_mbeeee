package auth

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/url"
	"strings"
	"time"
)

// SMTPConfig describes an SMTP relay. Port 465 uses implicit TLS; other ports
// upgrade with STARTTLS when the server offers it. User/Password are optional
// (a local catcher such as Mailpit needs none).
type SMTPConfig struct {
	Host, Port, User, Password string
	From                       string // e.g. "TenantKu <no-reply@example.com>"
	AppOrigin                  string
}

type SMTPMailer struct {
	cfg  SMTPConfig
	from mail.Address
}

func NewSMTPMailer(cfg SMTPConfig) (*SMTPMailer, error) {
	if cfg.Host == "" {
		return nil, errors.New("SMTP_HOST is required")
	}
	if cfg.Port == "" {
		cfg.Port = "587"
	}
	from, err := mail.ParseAddress(cfg.From)
	if err != nil {
		return nil, errors.New("MAIL_FROM must be an address such as \"TenantKu <no-reply@example.com>\"")
	}
	cfg.AppOrigin = strings.TrimSuffix(cfg.AppOrigin, "/")
	return &SMTPMailer{cfg: cfg, from: *from}, nil
}

func (m *SMTPMailer) SendVerification(ctx context.Context, toEmail, rawToken string) error {
	link := fmt.Sprintf("%s/verify-email?token=%s", m.cfg.AppOrigin, url.QueryEscape(rawToken))
	body := "Halo,\r\n\r\nTerima kasih sudah mendaftar di TenantKu. Verifikasi alamat email Anda lewat tautan berikut " +
		"(berlaku 24 jam):\r\n\r\n" + link + "\r\n\r\nJika Anda tidak merasa mendaftar, abaikan email ini.\r\n"
	return m.send(ctx, toEmail, "Verifikasi email TenantKu", body)
}

func (m *SMTPMailer) SendPasswordReset(ctx context.Context, toEmail, rawToken string) error {
	link := fmt.Sprintf("%s/reset-password?token=%s", m.cfg.AppOrigin, url.QueryEscape(rawToken))
	body := "Halo,\r\n\r\nKami menerima permintaan untuk mengatur ulang kata sandi akun TenantKu Anda. " +
		"Buka tautan berikut (berlaku 30 menit, sekali pakai):\r\n\r\n" + link +
		"\r\n\r\nJika Anda tidak memintanya, abaikan email ini. Kata sandi Anda tidak berubah.\r\n"
	return m.send(ctx, toEmail, "Atur ulang kata sandi TenantKu", body)
}

func (m *SMTPMailer) send(ctx context.Context, to, subject, body string) error {
	address, err := mail.ParseAddress(to)
	if err != nil || strings.ContainsAny(to, "\r\n") {
		return errors.New("invalid recipient")
	}

	var msgID [12]byte
	rand.Read(msgID[:])
	var msg strings.Builder
	header := func(k, v string) { msg.WriteString(k + ": " + v + "\r\n") }
	header("From", m.from.String())
	header("To", address.Address)
	header("Subject", mime.QEncoding.Encode("utf-8", subject))
	header("Date", time.Now().Format(time.RFC1123Z))
	header("Message-ID", "<"+hex.EncodeToString(msgID[:])+"@"+hostOf(m.from.Address)+">")
	header("MIME-Version", "1.0")
	header("Content-Type", `text/plain; charset="utf-8"`)
	header("Content-Transfer-Encoding", "quoted-printable")
	msg.WriteString("\r\n")
	qp := quotedprintable.NewWriter(&msg)
	qp.Write([]byte(body))
	qp.Close()

	return m.deliver(ctx, address.Address, msg.String())
}

func hostOf(addr string) string {
	if i := strings.LastIndex(addr, "@"); i >= 0 {
		return addr[i+1:]
	}
	return "localhost"
}

func (m *SMTPMailer) deliver(ctx context.Context, rcpt, msg string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	addr := net.JoinHostPort(m.cfg.Host, m.cfg.Port)
	tlsConfig := &tls.Config{ServerName: m.cfg.Host, MinVersion: tls.VersionTLS12}

	var conn net.Conn
	var err error
	if m.cfg.Port == "465" {
		conn, err = (&tls.Dialer{Config: tlsConfig}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("smtp connect: %w", err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}
	c, err := smtp.NewClient(conn, m.cfg.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("smtp greeting: %w", err)
	}
	defer c.Close()

	if ok, _ := c.Extension("STARTTLS"); ok && m.cfg.Port != "465" {
		if err := c.StartTLS(tlsConfig); err != nil {
			return fmt.Errorf("smtp starttls: %w", err)
		}
	}
	if m.cfg.User != "" {
		// PlainAuth refuses to send credentials over an unencrypted, non-local connection.
		if err := c.Auth(smtp.PlainAuth("", m.cfg.User, m.cfg.Password, m.cfg.Host)); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}
	if err := c.Mail(m.from.Address); err != nil {
		return fmt.Errorf("smtp sender: %w", err)
	}
	if err := c.Rcpt(rcpt); err != nil {
		return fmt.Errorf("smtp recipient: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	if _, err := w.Write([]byte(msg)); err != nil {
		return fmt.Errorf("smtp write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp send: %w", err)
	}
	return c.Quit()
}
