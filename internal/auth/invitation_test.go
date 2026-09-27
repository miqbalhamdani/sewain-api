package auth

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/miqbalhamdani/sewain-api/internal/owner"
	"github.com/miqbalhamdani/sewain-api/internal/queue"
)

// memTokens is the Redis surface, in a map.
//
// The token semantics under test are single-use and expiry, and both are
// properties of how this code calls SetNX and GetDel -- not of Redis. A real
// server would make the suite need one running to check a map lookup.
type memTokens struct {
	m map[string]string
}

func newMemTokens() *memTokens { return &memTokens{m: map[string]string{}} }

func (t *memTokens) SetNX(_ context.Context, key, value string, _ time.Duration) (bool, error) {
	if _, taken := t.m[key]; taken {
		return false, nil
	}
	t.m[key] = value
	return true, nil
}

func (t *memTokens) GetDel(_ context.Context, key string) (string, error) {
	v, ok := t.m[key]
	if !ok {
		return "", queue.ErrNotFound
	}
	delete(t.m, key)
	return v, nil
}

// capturedMail keeps the last body so a test can pull the link out of it,
// which is what a person does with their inbox.
type capturedMail struct{ to, subject, body string }

func (c *capturedMail) Send(_ context.Context, to, subject, body string) error {
	c.to, c.subject, c.body = to, subject, body
	return nil
}

// TestAcceptInvitation is the half of S1-084 that S1-010 left dangling: it
// created rows with status='invited' and no password, and nothing could turn
// one into an account.
func TestAcceptInvitation(t *testing.T) {
	ctx := t.Context()
	svc, store := newTestService(ctx, t)

	tokens := newMemTokens()
	mailbox := &capturedMail{}
	svc = svc.WithMail(tokens, mailbox, "http://localhost:3000")

	ownerID, _, _ := seedUser(ctx, t, store)
	ownerCtx := owner.NewContext(ctx, ownerID)

	email := "invitee-" + uuid.Must(uuid.NewV7()).String() + "@example.com"
	account, err := svc.InviteUser(ownerCtx, ownerID, email, "Operator Baru", RoleOperator)
	if err != nil {
		t.Fatalf("invite: %v", err)
	}

	t.Run("the invitation is actually sent", func(t *testing.T) {
		if mailbox.to != email {
			t.Errorf("mail went to %q, want %q", mailbox.to, email)
		}
		// The invitee has to know which rental is asking, and the name comes
		// from the database rather than the request -- a client-supplied one
		// would let anyone send a convincing invitation wearing somebody
		// else's business.
		if !contains(mailbox.subject, "Test owner") {
			t.Errorf("subject %q does not name the business", mailbox.subject)
		}
	})

	token := tokenFromLink(t, mailbox.body, "/accept-invitation?token=")

	t.Run("accepting activates and verifies in one step", func(t *testing.T) {
		session, err := svc.AcceptInvitation(ctx, token, "a password long enough")
		if err != nil {
			t.Fatalf("accept: %v", err)
		}
		if session.AccessToken == "" {
			t.Error("no session: the invitee would have to log in immediately after accepting")
		}
		// BR-006: accepting a link that only arrived by email IS the proof a
		// verification mail would ask for. Asking again is asking twice.
		if session.User.EmailVerifiedAt == nil {
			t.Error("email_verified_at is nil -- that means a second verification mail (BR-006)")
		}

		var status string
		var hash *string
		if err := store.InOwnerTx(ownerCtx, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx,
				`SELECT status, password_hash FROM users WHERE id = $1`, account.ID).
				Scan(&status, &hash)
		}); err != nil {
			t.Fatalf("read back: %v", err)
		}
		if status != "active" {
			t.Errorf("status = %q, want active", status)
		}
		if hash == nil {
			t.Error("password_hash is still null -- the account cannot log in")
		}
	})

	t.Run("the token is single use", func(t *testing.T) {
		_, err := svc.AcceptInvitation(ctx, token, "a different password")
		if err == nil {
			t.Fatal("the same invitation was accepted twice")
		}
	})

	t.Run("an unknown token is refused", func(t *testing.T) {
		if _, err := svc.AcceptInvitation(ctx, "made-up", "a password long enough"); err == nil {
			t.Error("an invented token was accepted")
		}
	})

	t.Run("the invitee can now log in", func(t *testing.T) {
		session, err := svc.Login(ctx, email, "a password long enough")
		if err != nil {
			t.Fatalf("login after accepting: %v", err)
		}
		if session.User.Role != RoleOperator {
			t.Errorf("role = %q, want operator", session.User.Role)
		}
	})
}

// TestVerificationTokenIsSingleUse covers the other token, whose flow is the
// same shape but whose failure would strand a fresh registration.
func TestVerificationTokenIsSingleUse(t *testing.T) {
	ctx := t.Context()
	svc, store := newTestService(ctx, t)

	tokens := newMemTokens()
	mailbox := &capturedMail{}
	svc = svc.WithMail(tokens, mailbox, "http://localhost:3000")

	ownerID, userID, _ := seedUser(ctx, t, store)

	// seedUser leaves the address verified; unset it, because an already
	// verified account is exactly the case SendVerification declines to mail.
	if err := store.InOwnerTx(owner.NewContext(ctx, ownerID), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE users SET email_verified_at = NULL WHERE id = $1`, userID)
		return err
	}); err != nil {
		t.Fatalf("unverify: %v", err)
	}

	if err := svc.SendVerification(ctx, userID, ownerID); err != nil {
		t.Fatalf("send verification: %v", err)
	}
	token := tokenFromLink(t, mailbox.body, "/verify-email?token=")

	if err := svc.VerifyEmail(ctx, token); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if err := svc.VerifyEmail(ctx, token); err == nil {
		t.Error("the same verification token was redeemed twice")
	}

	// Already verified: no second mail, because there is nothing to prove.
	mailbox.body = ""
	if err := svc.SendVerification(ctx, userID, ownerID); err != nil {
		t.Fatalf("send to a verified account: %v", err)
	}
	if mailbox.body != "" {
		t.Error("a verification mail was sent to an already verified address")
	}
}

func tokenFromLink(t *testing.T, body, marker string) string {
	t.Helper()
	i := indexOf(body, marker)
	if i < 0 {
		t.Fatalf("no %q in the mail body:\n%s", marker, body)
	}
	rest := body[i+len(marker):]
	if j := indexOf(rest, "\n"); j >= 0 {
		rest = rest[:j]
	}
	if rest == "" {
		t.Fatal("the link carries no token")
	}
	return rest
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func contains(s, sub string) bool { return indexOf(s, sub) >= 0 }
