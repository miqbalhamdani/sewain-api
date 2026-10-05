package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The two reads in this file are the only ones in the system that cross a
// owner boundary, and they are hand-written rather than generated so that they
// are visible as such -- generated queries all look alike, and these two must
// not blend in.
//
// Both go through a SECURITY DEFINER function owned by a role with BYPASSRLS
// (migration 000003_auth_lookup). The function fixes the returned columns at definition
// time; widening either is a schema change, a contract change, and a security
// review. Nothing else may read across owners from a request path -- BR-001.
//
// They exist because both callers run before an owner is known. Login has only
// an email; refresh has only a cookie. A plain query at that point matches zero
// rows, because FORCE RLS compares owner_id against a NULL setting.

// ErrNotFound is returned when a lookup matches nothing. Callers must not
// distinguish it from a wrong password in anything they send to a client.
var ErrNotFound = errors.New("not found")

// AuthUser is what the login path is allowed to learn about a user before it
// has authenticated them. Note what is absent: name, email, created_at.
type AuthUser struct {
	ID           uuid.UUID
	OwnerID      uuid.UUID
	PasswordHash *string // nil while an invitation is outstanding
	Status       string
	Role         string
}

// LookupUserForAuth resolves an email to the one user that owns it.
//
// Email is unique across the whole system (../docs/03-erd.md 3), which is what makes
// "one user" true and lets login carry no owner parameter.
func (s *Store) LookupUserForAuth(ctx context.Context, email string) (AuthUser, error) {
	var u AuthUser
	err := s.pool.QueryRow(ctx,
		`SELECT id, owner_id, password_hash, status, role FROM auth_lookup_user($1)`,
		email,
	).Scan(&u.ID, &u.OwnerID, &u.PasswordHash, &u.Status, &u.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		return AuthUser{}, ErrNotFound
	}
	if err != nil {
		return AuthUser{}, fmt.Errorf("look up user for auth: %w", err)
	}
	return u, nil
}

// AuthRefreshToken is what the refresh path learns before it has an owner.
// Deliberately carries no token material: the caller already holds the token,
// and the stored hash would be a verifier if it leaked.
type AuthRefreshToken struct {
	ID        uuid.UUID
	OwnerID   uuid.UUID
	UserID    uuid.UUID
	ExpiresAt time.Time
	RevokedAt *time.Time
}

// LookupRefreshToken resolves a token hash to its owner, so the rotation that
// follows can run inside InOwnerTx like any other write.
func (s *Store) LookupRefreshToken(ctx context.Context, tokenHash string) (AuthRefreshToken, error) {
	var t AuthRefreshToken
	err := s.pool.QueryRow(ctx,
		`SELECT id, owner_id, user_id, expires_at, revoked_at FROM auth_lookup_refresh_token($1)`,
		tokenHash,
	).Scan(&t.ID, &t.OwnerID, &t.UserID, &t.ExpiresAt, &t.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AuthRefreshToken{}, ErrNotFound
	}
	if err != nil {
		return AuthRefreshToken{}, fmt.Errorf("look up refresh token: %w", err)
	}
	return t, nil
}

// ActiveOwnerIDs lists every active rental, for the expiry sweep (S1-052).
//
// owners carries no RLS -- it is the tenant table itself -- so this is a plain
// read, not a third SECURITY DEFINER bypass. It returns ids only; every write
// the sweep makes still goes through InOwnerTx for one owner at a time.
func (s *Store) ActiveOwnerIDs(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx, `SELECT id FROM owners WHERE status = 'active' ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list active owners: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}
