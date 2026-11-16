package auth

import (
	"bufio"
	"context"
	"io"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"strings"
	"testing"
)

// fakeSMTP accepts one plain-text session and returns the DATA payload.
func fakeSMTP(t *testing.T) (port string, got chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	got = make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		say := func(s string) { io.WriteString(conn, s+"\r\n") }
		say("220 fake ESMTP")
		var data strings.Builder
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			cmd := strings.ToUpper(strings.TrimSpace(line))
			switch {
			case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
				say("250 fake")
			case strings.HasPrefix(cmd, "MAIL"), strings.HasPrefix(cmd, "RCPT"):
				say("250 ok")
			case cmd == "DATA":
				say("354 go")
				for {
					l, err := r.ReadString('\n')
					if err != nil || l == ".\r\n" {
						break
					}
					data.WriteString(l)
				}
				got <- data.String()
				say("250 queued")
			case cmd == "QUIT":
				say("221 bye")
				return
			}
		}
	}()
	_, port, _ = net.SplitHostPort(ln.Addr().String())
	return port, got
}

func TestSMTPMailerSendsLinkFromAppOrigin(t *testing.T) {
	port, got := fakeSMTP(t)
	m, err := NewSMTPMailer(SMTPConfig{
		Host: "127.0.0.1", Port: port, From: "TenantKu <no-reply@example.com>", AppOrigin: "https://app.example.com/",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SendPasswordReset(context.Background(), "user@example.com", "tok+en/1"); err != nil {
		t.Fatal(err)
	}
	msg, err := mail.ReadMessage(strings.NewReader(<-got))
	if err != nil {
		t.Fatal(err)
	}
	if msg.Header.Get("To") != "user@example.com" || !strings.Contains(msg.Header.Get("From"), "no-reply@example.com") {
		t.Fatalf("bad headers: %v", msg.Header)
	}
	body, _ := io.ReadAll(quotedprintable.NewReader(msg.Body))
	if !strings.Contains(string(body), "https://app.example.com/reset-password?token=tok%2Ben%2F1") {
		t.Fatalf("link missing or wrong origin:\n%s", body)
	}
}

func TestSMTPMailerRejectsBadConfigAndRecipient(t *testing.T) {
	if _, err := NewSMTPMailer(SMTPConfig{Host: "h", From: "not an address"}); err == nil {
		t.Fatal("invalid MAIL_FROM accepted")
	}
	m, _ := NewSMTPMailer(SMTPConfig{Host: "127.0.0.1", Port: "1", From: "a@example.com"})
	if err := m.SendVerification(context.Background(), "a@b.co\r\nBcc: x@y.z", "t"); err == nil {
		t.Fatal("header injection in recipient accepted")
	}
}
