// Package mail sends transactional email.  (S1-084)
//
// Phase 1 sends exactly two kinds: the verification link (BR-006) and the
// invitation link (BR-004). Both are short, both carry a single-use token, and
// neither has a template worth a template engine.
//
// The interface exists because the local sender and the production one are
// different things, and the backlog has no item that decides the production
// one yet -- that is M6's call, alongside the domain the mail would come from.
// Until then Mailpit takes delivery locally and the whole verification flow
// can be exercised end to end.
package mail

import (
	"context"
	"fmt"
	"log/slog"
	"net/smtp"
	"strings"
)

// Mailer sends one message. Plain text, one recipient.
//
// No attachments, no HTML, no cc: every one of those is a feature that phase 1
// does not send, and an interface that promises them invites somebody to use
// them before there is a sender that can.
type Mailer interface {
	Send(ctx context.Context, to, subject, body string) error
}

// SMTP sends through a plain SMTP server with no authentication and no TLS.
//
// That combination is only safe on a loopback address, which is exactly where
// Mailpit runs. New refuses anything else, so the shape of this type cannot be
// quietly pointed at a real relay.
type SMTP struct {
	addr string
	from string
}

// NewSMTP fails on a non-loopback address rather than accepting one.
func NewSMTP(addr, from string) (*SMTP, error) {
	host, _, found := strings.Cut(addr, ":")
	if !found {
		return nil, fmt.Errorf("smtp address needs a port, got %q", addr)
	}
	if host != "localhost" && host != "127.0.0.1" && host != "::1" {
		return nil, fmt.Errorf(
			"smtp host %q is not loopback: this sender has no auth and no TLS, "+
				"and a production relay needs both (M6 decides which)", host)
	}
	return &SMTP{addr: addr, from: from}, nil
}

func (s *SMTP) Send(ctx context.Context, to, subject, body string) error {
	msg := "From: " + s.from + "\r\n" +
		"To: " + to + "\r\n" +
		"Subject: " + subject + "\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" + body + "\r\n"

	if err := smtp.SendMail(s.addr, nil, s.from, []string{to}, []byte(msg)); err != nil {
		return fmt.Errorf("send mail to %s: %w", to, err)
	}
	slog.InfoContext(ctx, "mail sent", "to", to, "subject", subject)
	return nil
}

// Discard drops every message and says so in the log.
//
// For tests, and for a developer who has not started Mailpit: a registration
// that fails because no mail server is listening would be a worse first
// experience than one that succeeds with the link in the log.
type Discard struct{}

func (Discard) Send(ctx context.Context, to, subject, body string) error {
	slog.InfoContext(ctx, "mail discarded -- no sender configured",
		"to", to, "subject", subject, "body", body)
	return nil
}
