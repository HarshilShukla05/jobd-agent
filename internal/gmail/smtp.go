package gmail

import (
	"context"
	"fmt"
	"mime"
	"net/smtp"
	"os"
	"strings"
)

// SMTPSender sends through Gmail's SMTP endpoint using an App Password.
// This is the default path because it needs no OAuth client (which Google
// only lets you create by hand in the console) — Harshil generates one
// App Password at myaccount.google.com/apppasswords and pastes it into
// ~/.config/jobd/env as GMAIL_APP_PASSWORD.
type SMTPSender struct {
	from string
	auth smtp.Auth
}

const smtpHost = "smtp.gmail.com"

func NewSMTPSender(from string) (*SMTPSender, error) {
	LoadEnvFile()
	pass := strings.ReplaceAll(os.Getenv("GMAIL_APP_PASSWORD"), " ", "")
	if pass == "" {
		return nil, fmt.Errorf(
			"GMAIL_APP_PASSWORD not set.\n" +
				"  1. Turn on 2-Step Verification: https://myaccount.google.com/signinoptions/twosv\n" +
				"  2. Create an App Password (\"Mail\"): https://myaccount.google.com/apppasswords\n" +
				"  3. Add to ~/.config/jobd/env:  GMAIL_APP_PASSWORD=<the 16 characters>")
	}
	if from == "" {
		return nil, fmt.Errorf("From address is empty")
	}
	return &SMTPSender{from: from, auth: smtp.PlainAuth("", from, pass, smtpHost)}, nil
}

func (s *SMTPSender) Send(ctx context.Context, to, subject, body string) (string, error) {
	var msg strings.Builder
	fmt.Fprintf(&msg, "From: %s\r\n", s.from)
	fmt.Fprintf(&msg, "To: %s\r\n", to)
	// RFC 2047 encode so non-ASCII subjects (em dashes etc.) survive
	fmt.Fprintf(&msg, "Subject: %s\r\n", mime.QEncoding.Encode("UTF-8", subject))
	msg.WriteString("MIME-Version: 1.0\r\n")
	msg.WriteString("Content-Type: text/plain; charset=UTF-8\r\n\r\n")
	msg.WriteString(body)

	if err := smtp.SendMail(smtpHost+":587", s.auth, s.from, []string{to}, []byte(msg.String())); err != nil {
		return "", fmt.Errorf("smtp send: %w", err)
	}
	return "smtp", nil
}

// LoadEnvFile reads KEY=VALUE lines from ~/.config/jobd/env into the process
// environment (existing vars win).
func LoadEnvFile() {
	home, _ := os.UserHomeDir()
	b, err := os.ReadFile(home + "/.config/jobd/env")
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if ok && k != "" && os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
}

// Sender is the send boundary — SMTP today, OAuth Gmail API if ever needed.
type Sender interface {
	Send(ctx context.Context, to, subject, body string) (string, error)
}

// NewSender picks SMTP (App Password) when available, else the Gmail API.
func NewSender(ctx context.Context, from string) (Sender, error) {
	if s, err := NewSMTPSender(from); err == nil {
		return s, nil
	} else if _, statErr := os.Stat(configPath("gmail-token.json")); statErr != nil {
		return nil, err // neither path configured: report the simpler one
	}
	return NewAPISender(ctx, from)
}
