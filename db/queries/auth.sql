-- Queries over the identity tables. Every one of these runs INSIDE InOwnerTx.
--
-- The two reads that run BEFORE a rental is known are not here. They are
-- hand-written in internal/db/auth_lookup.go so that the only two reads in the
-- system that reach past owner scoping are visible as such, rather than sitting
-- among generated code that all looks alike.

-- name: GetSession :one
-- Everything the Session response needs, in one round trip. Runs inside
-- InOwnerTx, so RLS has already scoped users; owners has no RLS by design.
--
-- u.status is here for Refresh, not for the response body. Login checks the
-- status against auth_lookup_user, but Refresh reached this row for the role
-- alone and never looked -- so a disabled account kept minting access tokens
-- every 15 minutes, forever. BR-004 caps a revoked session at the access token
-- TTL, and that promise needs the column (S1-010).
SELECT u.id AS user_id, u.name AS user_name, u.role, u.status, u.email_verified_at,
       o.id AS owner_id, o.name AS owner_name, o.slug, o.business_type
  FROM users u
  JOIN owners o ON o.id = u.owner_id
 WHERE u.id = $1;

-- name: TouchLastLogin :exec
UPDATE users SET last_login_at = now() WHERE id = $1;

-- name: CreateRefreshToken :exec
INSERT INTO refresh_tokens (id, owner_id, user_id, token_hash, rotated_from, expires_at)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: RevokeRefreshToken :execrows
-- One token, by id. Used by logout, which has no reason to touch any other.
UPDATE refresh_tokens SET revoked_at = now()
 WHERE id = $1 AND revoked_at IS NULL;

-- name: HasSuccessor :one
-- Whether some other token was created by rotating this one.
--
-- This is what separates theft from an ordinary sign-out. Both leave a token
-- with revoked_at set, so revoked_at alone cannot tell them apart -- and
-- treating a sign-out as theft means a stale browser tab replaying its old
-- cookie signs the user out of every other device.
SELECT EXISTS (SELECT 1 FROM refresh_tokens WHERE rotated_from = $1);

-- name: RevokeAllUserTokens :execrows
-- Used when an already-rotated token is presented again, which means it was
-- stolen: the whole rotation chain is revoked and the user is signed out
-- everywhere (../docs/04-api-spec.md 3.1).
--
-- This revokes every active token for the user, not only the chain the reused
-- token belongs to. A chain starts at each login, so a user signed in on a
-- phone and a laptop has two -- and revoking one of them would leave the
-- thief's other session alive while claiming the user was signed out
-- everywhere. The chain is a subset of this.
UPDATE refresh_tokens SET revoked_at = now()
 WHERE user_id = $1 AND revoked_at IS NULL;

-- name: InsertApiKey :one
-- S1-079. The secret never reaches this table: key_hash is argon2id.
INSERT INTO api_keys (id, owner_id, name, key_prefix, key_hash, rate_limit_per_min, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING id, name, key_prefix, rate_limit_per_min, last_used_at, revoked_at, created_at;

-- name: ListApiKeys :many
SELECT id, name, key_prefix, rate_limit_per_min, last_used_at, revoked_at, created_at
  FROM api_keys
 ORDER BY created_at DESC;

-- name: RevokeApiKey :execrows
-- Revoked, never deleted (BR-031). Revoking twice is still revoked.
UPDATE api_keys SET revoked_at = COALESCE(revoked_at, now()) WHERE id = $1;

-- name: TouchApiKey :exec
-- last_used_at, at most once a minute per key: a write per request would turn
-- every public read into a row lock.
UPDATE api_keys SET last_used_at = now()
 WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute');
