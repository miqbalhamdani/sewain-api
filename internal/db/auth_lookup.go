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

// TenantOwner is what the Host lookup needs to decide a public request: who,
// whether they are open for business, and whether their public page is live.
type TenantOwner struct {
	ID     uuid.UUID
	Active bool
	// Live is BR-096: slug, whatsapp and address all set. The portal needs
	// only an active owner; the public catalogue needs Live as well.
	Live bool
}

// OwnerBySlug resolves <slug>.<apex> to its rental (BR-030) -- the hottest
// lookup on the public surface, served by owners_slug_unique. Like
// ActiveOwnerIDs, a plain read: owners carries no RLS.
func (s *Store) OwnerBySlug(ctx context.Context, slug string) (TenantOwner, error) {
	var o TenantOwner
	err := s.pool.QueryRow(ctx, `
		SELECT id, status = 'active', whatsapp IS NOT NULL AND address IS NOT NULL
		  FROM owners WHERE slug = $1`, slug).Scan(&o.ID, &o.Active, &o.Live)
	if err != nil {
		return TenantOwner{}, fmt.Errorf("owner by slug: %w", err)
	}
	return o, nil
}

// APIKeyLookup is what auth_lookup_api_key returns: enough to verify a
// presented key and pick its owner, nothing to display (BR-031).
type APIKeyLookup struct {
	ID, OwnerID     uuid.UUID
	KeyHash         string
	RateLimitPerMin int
	Revoked         bool
}

// LookupAPIKey is the third pre-owner read (000018): a key on api.<apex>
// arrives with no owner context, and api_keys is under RLS.
func (s *Store) LookupAPIKey(ctx context.Context, prefix string) (APIKeyLookup, error) {
	var k APIKeyLookup
	err := s.pool.QueryRow(ctx, `
		SELECT id, owner_id, key_hash, rate_limit_per_min, revoked_at IS NOT NULL
		  FROM auth_lookup_api_key($1)`, prefix).
		Scan(&k.ID, &k.OwnerID, &k.KeyHash, &k.RateLimitPerMin, &k.Revoked)
	if err != nil {
		return APIKeyLookup{}, fmt.Errorf("lookup api key: %w", err)
	}
	return k, nil
}

// OwnerByID is OwnerBySlug for the external lane, where the key named the owner.
func (s *Store) OwnerByID(ctx context.Context, id uuid.UUID) (TenantOwner, error) {
	var o TenantOwner
	err := s.pool.QueryRow(ctx, `
		SELECT id, status = 'active', slug IS NOT NULL AND whatsapp IS NOT NULL AND address IS NOT NULL
		  FROM owners WHERE id = $1`, id).Scan(&o.ID, &o.Active, &o.Live)
	if err != nil {
		return TenantOwner{}, fmt.Errorf("owner by id: %w", err)
	}
	return o, nil
}

// OriginAllowed reports whether origin is in an owner's allowed_origins --
// one owner's when ownerID is set, any active owner's for a preflight, which
// carries no key to name one (04-api-spec.md §4.1).
func (s *Store) OriginAllowed(ctx context.Context, ownerID *uuid.UUID, origin string) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM owners
		                WHERE status = 'active' AND $2 = ANY(allowed_origins)
		                  AND ($1::uuid IS NULL OR id = $1::uuid))`, ownerID, origin).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("origin allowed: %w", err)
	}
	return ok, nil
}
