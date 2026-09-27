package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/miqbalhamdani/sewain-api/internal/db/sqlcgen"
	"github.com/miqbalhamdani/sewain-api/internal/owner"
	apperrors "github.com/miqbalhamdani/sewain-api/internal/platform/errors"
	"github.com/miqbalhamdani/sewain-api/internal/queue"
)

// Email verification and invitation acceptance.  (S1-084, BR-004, BR-006)
//
// Tokens live in Redis, not a table (03-erd.md §4). They are single-use and
// expire on their own, so a table would only accumulate dead rows needing a
// cleanup job -- and S1-059 was deferred precisely because cleanup jobs are
// expensive to own.
//
// Only the hash is stored. A Redis dump, a log line, or a support screenshot
// then contains nothing that can be redeemed, which is the same reasoning that
// puts only a hash of the refresh token in the database.

const (
	// 24 hours, and expiry is not a dead end: resend is always available,
	// because verifying itself is the only thing an unverified account can do.
	verificationTTL = 24 * time.Hour

	// Longer, because an invitation waits on a person who did not ask for it
	// and may not read mail for a week. There is no resend-invitation endpoint
	// yet, so a short window here would strand the account (S1-010).
	invitationTTL = 7 * 24 * time.Hour
)

// TokenStore is the Redis surface this file needs, declared in the consumer.
type TokenStore interface {
	SetNX(ctx context.Context, key, value string, ttl time.Duration) (bool, error)
	GetDel(ctx context.Context, key string) (string, error)
}

// Mailer is what sends the link. Declared here rather than imported so the
// domain does not depend on a particular sender.
type Mailer interface {
	Send(ctx context.Context, to, subject, body string) error
}

// WithMail attaches the token store and sender. Separate from NewService
// because Login, Refresh and Logout need neither, and a constructor that
// demands both would make every auth test set up Redis and a mailbox.
func (s *Service) WithMail(tokens TokenStore, mailer Mailer, baseURL string) *Service {
	s.tokens = tokens
	s.mailer = mailer
	s.baseURL = baseURL
	return s
}

// newToken returns the token to put in a link and the key to store it under.
//
// 32 bytes from crypto/rand, URL-safe. The key is the SHA-256 of the token, so
// what is stored cannot be replayed.
func newToken(prefix string) (token, key string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("generate token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(raw)
	return token, tokenKey(prefix, token), nil
}

func tokenKey(prefix, token string) string {
	sum := sha256.Sum256([]byte(token))
	return prefix + ":" + hex.EncodeToString(sum[:])
}

// The stored value carries the owner alongside the user.
//
// Both routes that redeem a token are unauthenticated -- the link is opened
// from a mail client, which may not even be the browser that registered -- so
// there is no owner in the context and no token to derive one from. Looking it
// up would need a third SECURITY DEFINER bypass, and 03-erd.md §3 closes that
// door explicitly: "Tidak akan ada yang ketiga."
//
// Putting the owner in the value costs nothing. It is already behind a hashed,
// single-use, expiring key, and it is an id the redeemer is about to act as
// anyway.
func packSubject(ownerID, userID uuid.UUID) string { return ownerID.String() + ":" + userID.String() }

func unpackSubject(raw string) (ownerID, userID uuid.UUID, err error) {
	o, u, found := strings.Cut(raw, ":")
	if !found {
		return uuid.Nil, uuid.Nil, errors.New("malformed token subject")
	}
	if ownerID, err = uuid.Parse(o); err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	if userID, err = uuid.Parse(u); err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	return ownerID, userID, nil
}

// SendVerification mints a link and mails it.
//
// Errors from the mailer are returned, not swallowed: a registration that
// reports success while the only way out of the verification wall never
// arrives is worse than one that fails loudly.
func (s *Service) SendVerification(ctx context.Context, userID uuid.UUID, ownerID uuid.UUID) error {
	var row sqlcgen.GetUserEmailRow
	if err := s.store.InOwnerTx(owner.NewContext(ctx, ownerID), func(tx pgx.Tx) error {
		var err error
		row, err = sqlcgen.New(tx).GetUserEmail(ctx, userID)
		return err
	}); err != nil {
		return fmt.Errorf("read user for verification: %w", err)
	}
	if row.EmailVerifiedAt != nil {
		return nil // already verified; sending again would be noise
	}

	token, key, err := newToken("verify")
	if err != nil {
		return err
	}
	if _, err := s.tokens.SetNX(ctx, key, packSubject(ownerID, userID), verificationTTL); err != nil {
		return err
	}

	return s.mailer.Send(ctx, row.Email, "Verifikasi email Sewain",
		"Halo "+row.Name+",\n\n"+
			"Klik tautan ini untuk memverifikasi email kamu:\n\n"+
			s.baseURL+"/verify-email?token="+token+"\n\n"+
			"Tautannya berlaku 24 jam dan hanya bisa dipakai sekali.\n"+
			"Kalau kedaluwarsa, minta kirim ulang dari layar verifikasi.\n")
}

// VerifyEmail redeems a verification token.
//
// GetDel, so redeeming is atomic: read-then-delete as two calls lets two
// requests interleave and use the same token twice, and single-use has to hold
// under concurrency or it does not hold.
func (s *Service) VerifyEmail(ctx context.Context, token string) error {
	raw, err := s.tokens.GetDel(ctx, tokenKey("verify", token))
	if errors.Is(err, queue.ErrNotFound) {
		return invalidToken()
	}
	if err != nil {
		return err
	}
	ownerID, userID, err := unpackSubject(raw)
	if err != nil {
		return invalidToken()
	}

	return s.store.InOwnerTx(owner.NewContext(ctx, ownerID), func(tx pgx.Tx) error {
		// Zero rows means already verified. Not an error: the person clicked
		// the link twice, and telling them off for that helps nobody.
		_, err := sqlcgen.New(tx).MarkEmailVerified(ctx, userID)
		return err
	})
}

// ResendVerification is always available. Verifying is the only thing an
// unverified account can do, so closing this closes the only way out.
func (s *Service) ResendVerification(ctx context.Context, userID, ownerID uuid.UUID) error {
	return s.SendVerification(ctx, userID, ownerID)
}

// SendInvitation mails the link that turns an `invited` row into a usable
// account. Called by InviteUser (S1-010).
func (s *Service) SendInvitation(ctx context.Context, ownerID, userID uuid.UUID, email, name, businessName string) error {
	token, key, err := newToken("invite")
	if err != nil {
		return err
	}
	if _, err := s.tokens.SetNX(ctx, key, packSubject(ownerID, userID), invitationTTL); err != nil {
		return err
	}

	return s.mailer.Send(ctx, email, "Undangan bergabung di "+businessName,
		"Halo "+name+",\n\n"+
			"Kamu diundang bergabung di "+businessName+" pada Sewain.\n"+
			"Klik tautan ini untuk memasang password dan mulai memakainya:\n\n"+
			s.baseURL+"/accept-invitation?token="+token+"\n\n"+
			"Tautannya berlaku 7 hari dan hanya bisa dipakai sekali.\n")
}

// AcceptInvitation sets a password, activates the account, and marks the email
// verified -- all three, in one statement.
//
// The third is not a shortcut. An invitation can only be accepted through the
// link that arrived by email, so accepting it already proves control of the
// address. That is the same proof a verification mail asks for, and sending
// one afterwards asks for it twice (BR-004, BR-006).
func (s *Service) AcceptInvitation(ctx context.Context, token, password string) (Session, error) {
	if len(password) < 8 {
		return Session{}, apperrors.ValidationFailed("password is at least 8 characters.")
	}

	raw, err := s.tokens.GetDel(ctx, tokenKey("invite", token))
	if errors.Is(err, queue.ErrNotFound) {
		return Session{}, invalidToken()
	}
	if err != nil {
		return Session{}, err
	}
	ownerID, userID, err := unpackSubject(raw)
	if err != nil {
		return Session{}, invalidToken()
	}

	hash, err := HashPassword(password)
	if err != nil {
		return Session{}, fmt.Errorf("hash password: %w", err)
	}

	ownerCtx := owner.NewContext(ctx, ownerID)
	var affected int64
	if err := s.store.InOwnerTx(ownerCtx, func(tx pgx.Tx) error {
		var err error
		affected, err = sqlcgen.New(tx).AcceptInvitation(ctx, sqlcgen.AcceptInvitationParams{
			ID: userID, PasswordHash: &hash,
		})
		return err
	}); err != nil {
		return Session{}, fmt.Errorf("accept invitation: %w", err)
	}
	if affected == 0 {
		// The row is no longer `invited`: already accepted, or disabled before
		// it ever was. The token is spent either way.
		return Session{}, invalidToken()
	}

	return s.issue(ownerCtx, ownerID, userID, nil)
}

// invalidToken is one answer for unknown, expired and already-used.
//
// Telling them apart would say whether a token ever existed, and the only
// caller who benefits from that distinction is one guessing at tokens.
func invalidToken() error {
	return &apperrors.Error{
		Code:   apperrors.CodeVerificationTokenInvalid,
		Status: 422,
		Title:  "Verification link is not usable",
		Detail: "That link is expired, already used, or not recognised. Request a new one.",
	}
}
