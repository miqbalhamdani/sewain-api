-- name: CreateOwner :exec
-- The only statement in the system that creates a business (BR-005). slug is
-- absent on purpose: it is optional, paid, and filled in later from
-- PATCH /settings, and asking for it at registration is one more place to stop.
INSERT INTO owners (id, name, business_type) VALUES ($1, $2, $3);

-- name: CreateOwnerUser :exec
-- Runs in the same transaction as CreateOwner. A business without an owner is
-- a row nobody can ever log into, so the two are one write or neither is.
INSERT INTO users (id, owner_id, email, password_hash, name, role, status)
VALUES ($1, $2, $3, $4, $5, 'owner', 'active');

-- name: MarkEmailVerified :execrows
-- Idempotent by the WHERE clause: verifying twice is still verified, and the
-- second call must not move the timestamp.
UPDATE users SET email_verified_at = now(), updated_at = now()
 WHERE id = $1 AND email_verified_at IS NULL;

-- name: AcceptInvitation :execrows
-- The three things an invitation acceptance sets, in one statement so none can
-- land without the others. email_verified_at is included because accepting a
-- link that only arrived by email IS the proof a verification mail would ask
-- for -- asking again is asking twice (BR-004, BR-006).
UPDATE users SET
    password_hash     = $2,
    status            = 'active',
    email_verified_at = COALESCE(email_verified_at, now()),
    updated_at        = now()
 WHERE id = $1 AND status = 'invited';

-- name: GetUserEmail :one
SELECT email, name, email_verified_at FROM users WHERE id = $1;

-- name: GetOwnerName :one
-- For the invitation mail. owners has no RLS (it IS the tenant), so the WHERE
-- clause is the isolation -- and the id comes from the owner context.
SELECT name FROM owners WHERE id = $1;
