// Package auth handles signing in, keeping a session alive, and signing out.
package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/miqbalhamdani/sewain-api/internal/db"
	"github.com/miqbalhamdani/sewain-api/internal/db/sqlcgen"
	"github.com/miqbalhamdani/sewain-api/internal/owner"
)

// ErrUnauthenticated is the single failure this package reports outward.
//
// A wrong password, an unknown email, a disabled account, an expired token and
// a stolen one all collapse into it on purpose. Any difference a client can
// observe -- a different code, a different message, a measurably different
// response time -- tells an attacker which emails exist and which are worth
// pursuing.
var ErrUnauthenticated = errors.New("unauthenticated")

// Session is what login and refresh return.
type Session struct {
	AccessToken  string
	ExpiresIn    int
	RefreshToken string // goes into the cookie, never into the response body
	User         SessionUser
	Owner        SessionOwner
}

type SessionUser struct {
	ID          uuid.UUID
	Name        string
	Role        string
	Permissions []string
}

// SessionOwner is the rental this user works in. Exactly one, always -- there is
// no list and no switcher (BR-004).
//
// No Currency: money is always full rupiah as an integer, so a currency field
// would have exactly one value forever. No Timezone: the wire is always UTC and
// Asia/Jakarta is a rendering decision in the frontend. Both are fields
// new-commerce needs and this does not.
type SessionOwner struct {
	ID   uuid.UUID
	Name string
	Slug string
}

// Service is the auth use cases. Everything it does that touches an owner's
// rows goes through InOwnerTx; the two lookups that cannot are in
// internal/db/auth_lookup.go and are the system's only cross-owner reads.
type Service struct {
	store  *db.Store
	signer *Signer
	now    func() time.Time
}

func NewService(store *db.Store, signer *Signer) *Service {
	return &Service{store: store, signer: signer, now: time.Now}
}

// Login exchanges an email and password for a session.
func (s *Service) Login(ctx context.Context, email, password string) (Session, error) {
	user, err := s.store.LookupUserForAuth(ctx, email)
	if errors.Is(err, db.ErrNotFound) {
		// Still spend the time an argon2id verification would take. Returning
		// immediately on an unknown email makes "does this address have an
		// account" answerable with a stopwatch.
		_ = VerifyPassword(dummyHash, password)
		return Session{}, ErrUnauthenticated
	}
	if err != nil {
		return Session{}, err
	}

	// A user mid-invitation has no password set; there is nothing to verify
	// against. Same spend, same answer.
	if user.PasswordHash == nil {
		_ = VerifyPassword(dummyHash, password)
		return Session{}, ErrUnauthenticated
	}
	if err := VerifyPassword(*user.PasswordHash, password); err != nil {
		return Session{}, ErrUnauthenticated
	}
	// Checked after the password, not before: an early return here would make
	// a disabled account distinguishable from a wrong one.
	if user.Status != "active" {
		return Session{}, ErrUnauthenticated
	}

	return s.issue(ctx, user.OwnerID, user.ID, user.Role, nil)
}

// Refresh rotates a refresh token and issues a new access token.
//
// This is where the acceptance for P1-011 lives: presenting a token that was
// already rotated means it was stolen, and the response is to end every session
// the user has -- not to reject the one request.
func (s *Service) Refresh(ctx context.Context, presented string) (Session, error) {
	stored, err := s.store.LookupRefreshToken(ctx, HashRefreshToken(presented))
	if errors.Is(err, db.ErrNotFound) {
		return Session{}, ErrUnauthenticated
	}
	if err != nil {
		return Session{}, err
	}

	ownerCtx := owner.NewContext(ctx, stored.OwnerID)

	if stored.RevokedAt != nil {
		// A revoked token was either rotated away or signed out, and the two
		// need very different answers. Only a rotation leaves a successor
		// behind, so that is what tells them apart.
		//
		// Without this distinction a stale browser tab replaying its old cookie
		// after a sign-out would look exactly like a theft, and would sign the
		// user out of every other device.
		var superseded bool
		if err := s.store.InOwnerTx(ownerCtx, func(tx pgx.Tx) error {
			q := sqlcgen.New(tx)
			var err error
			if superseded, err = q.HasSuccessor(ctx, &stored.ID); err != nil {
				return err
			}
			if !superseded {
				return nil
			}
			// Rotated away, and presented anyway. The legitimate holder would
			// be carrying the replacement, so whoever sent this has an old
			// copy -- which means a copy was taken.
			//
			// Every active token for the user goes, not only this chain: a
			// chain begins at each login, so someone signed in on a phone and a
			// laptop has two, and revoking one would leave the thief's other
			// session alive while claiming they were signed out everywhere.
			// API spec.md 2 asks for both.
			_, err = q.RevokeAllUserTokens(ctx, stored.UserID)
			return err
		}); err != nil {
			return Session{}, fmt.Errorf("handle revoked refresh token: %w", err)
		}
		return Session{}, ErrUnauthenticated
	}

	if s.now().After(stored.ExpiresAt) {
		return Session{}, ErrUnauthenticated
	}

	var role string
	if err := s.store.InOwnerTx(ownerCtx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		// Revoke first. If this affects no rows, another request rotated the
		// same token between the lookup and here, and continuing would leave
		// two live tokens where there should be one.
		n, err := q.RevokeRefreshToken(ctx, stored.ID)
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrUnauthenticated
		}
		row, err := q.GetSession(ctx, stored.UserID)
		if err != nil {
			return err
		}
		role = row.Role
		return nil
	}); err != nil {
		if errors.Is(err, ErrUnauthenticated) {
			return Session{}, ErrUnauthenticated
		}
		return Session{}, err
	}

	return s.issue(ctx, stored.OwnerID, stored.UserID, role, &stored.ID)
}

// Me returns the current session context: the rental, the role, and what that
// role may do.
//
// It runs inside InOwnerTx like any other read, so RLS scopes `users` to the
// rental in the caller's token. A user id from a valid token for a different
// rental therefore finds no row and gets ErrUnauthenticated -- not because the
// handler checked, but because the policy did.
func (s *Service) Me(ctx context.Context, userID uuid.UUID) (SessionUser, SessionOwner, error) {
	var user SessionUser
	var owner SessionOwner

	err := s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		row, err := sqlcgen.New(tx).GetSession(ctx, userID)
		if err != nil {
			return err
		}
		user = SessionUser{
			ID:          row.UserID,
			Name:        row.UserName,
			Role:        row.Role,
			Permissions: PermissionsFor(row.Role),
		}
		owner = SessionOwner{ID: row.OwnerID, Name: row.OwnerName, Slug: row.Slug}
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return SessionUser{}, SessionOwner{}, ErrUnauthenticated
	}
	if err != nil {
		return SessionUser{}, SessionOwner{}, err
	}
	return user, owner, nil
}

// Logout revokes the presented refresh token.
//
// Silent on every failure. There is nothing useful to tell a caller about a
// session that is already gone, and an error here would let someone probe which
// tokens are live.
func (s *Service) Logout(ctx context.Context, presented string) error {
	if presented == "" {
		return nil
	}
	stored, err := s.store.LookupRefreshToken(ctx, HashRefreshToken(presented))
	if err != nil {
		return nil //nolint:nilerr // deliberate: an unknown token is not an error to report
	}
	return s.store.InOwnerTx(owner.NewContext(ctx, stored.OwnerID), func(tx pgx.Tx) error {
		_, err := sqlcgen.New(tx).RevokeRefreshToken(ctx, stored.ID)
		return err
	})
}

// issue mints an access token and a fresh refresh token, and records the new
// refresh token as a rotation of rotatedFrom when there is one.
func (s *Service) issue(ctx context.Context, ownerID, userID uuid.UUID, role string, rotatedFrom *uuid.UUID) (Session, error) {
	now := s.now()

	access, err := s.signer.Issue(userID, ownerID, role, now)
	if err != nil {
		return Session{}, err
	}
	refresh, refreshHash, err := NewRefreshToken()
	if err != nil {
		return Session{}, err
	}

	// uuid.NewV7 rather than gen_random_uuid: time-ordered keys keep B-tree
	// inserts append-only. CLAUDE.md.
	tokenID, err := uuid.NewV7()
	if err != nil {
		return Session{}, fmt.Errorf("generate token id: %w", err)
	}

	var out Session
	err = s.store.InOwnerTx(owner.NewContext(ctx, ownerID), func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		if err := q.CreateRefreshToken(ctx, sqlcgen.CreateRefreshTokenParams{
			ID:          tokenID,
			OwnerID:     ownerID,
			UserID:      userID,
			TokenHash:   refreshHash,
			RotatedFrom: rotatedFrom,
			ExpiresAt:   now.Add(RefreshTokenTTL),
		}); err != nil {
			return err
		}
		if rotatedFrom == nil {
			if err := q.TouchLastLogin(ctx, userID); err != nil {
				return err
			}
		}

		row, err := q.GetSession(ctx, userID)
		if err != nil {
			return err
		}
		out = Session{
			AccessToken:  access,
			ExpiresIn:    int(AccessTokenTTL.Seconds()),
			RefreshToken: refresh,
			User: SessionUser{
				ID:   row.UserID,
				Name: row.UserName,
				Role: row.Role,
				// The client hides actions it does not find here rather than
				// disabling them, so this list is part of the UI contract and
				// not merely informational.
				Permissions: PermissionsFor(row.Role),
			},
			Owner: SessionOwner{
				ID:   row.OwnerID,
				Name: row.OwnerName,
				Slug: row.Slug,
			},
		}
		return nil
	})
	if err != nil {
		return Session{}, err
	}
	return out, nil
}

// dummyHash is a real argon2id hash of a value nobody knows, verified against
// when there is no user, so that the failing path costs the same as the
// succeeding one.
const dummyHash = "$argon2id$v=19$m=65536,t=3,p=4$c29tZXNhbHR2YWx1ZXg$Zm9yY2luZ2NvbnN0YW50dGltZWNvc3Rvbmx5MDA"
