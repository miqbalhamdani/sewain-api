-- Every query here runs inside InOwnerTx, so RLS has already scoped `users` to
-- one rental. That is what makes the id-taking queries return zero rows for
-- another owner's id -- no ownership check in Go, and none wanted (BR-001).

-- name: ListUsers :many
SELECT id, email, name, role, status, last_login_at
  FROM users
 ORDER BY created_at;

-- name: InviteUser :one
-- No password_hash: the row exists so the invitee has something to accept.
-- status defaults to 'invited' in the schema and is not settable from here.
INSERT INTO users (id, owner_id, email, name, role)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, email, name, role, status, last_login_at;

-- name: UpdateUser :one
-- COALESCE for the same reason as UpdateSettings: absent key, value stands.
UPDATE users SET
    role       = COALESCE(sqlc.narg(role),   role),
    status     = COALESCE(sqlc.narg(status), status),
    updated_at = now()
 WHERE id = sqlc.arg(id)
RETURNING id, email, name, role, status, last_login_at;

-- name: DisableUser :execrows
-- Not a DELETE, and there is no query in this file that is. created_by columns
-- elsewhere have to stay explicable, so the row outlives the account.
UPDATE users SET status = 'disabled', updated_at = now()
 WHERE id = $1 AND status <> 'disabled';

-- name: LockActiveOwners :many
-- FOR UPDATE so two owners demoting each other at once queue up: the second
-- re-reads after the first commits and sees one owner left (S1-067).
SELECT id FROM users WHERE role = 'owner' AND status = 'active' FOR UPDATE;

-- name: UserExists :one
-- Distinguishes "already disabled" (204, idempotent) from "not in this rental"
-- (404). DisableUser alone cannot: both return zero rows.
SELECT EXISTS (SELECT 1 FROM users WHERE id = $1);
