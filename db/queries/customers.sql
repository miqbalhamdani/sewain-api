-- Penyewa dan jejak audit.  (S1-020, S1-021)
--
-- Every query runs inside InOwnerTx; RLS scopes the rows. owner_id appears only
-- as an INSERT value.

-- name: CreateCustomer :exec
INSERT INTO customers (id, owner_id, created_by, name, phone, id_type,
                       id_number_enc, id_number_last4)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: GetCustomer :one
SELECT id, name, phone, id_type, id_number_last4, is_blacklisted,
       blacklist_reason, created_at
  FROM customers
 WHERE id = $1 AND deleted_at IS NULL;

-- name: ListCustomers :many
-- Keyset on id: UUID v7 is creation order, so "newest first" and the cursor are
-- the same column. q matches name or phone, case-insensitively -- the booking
-- form searches while the operator types.
SELECT id, name, phone, id_type, id_number_last4, is_blacklisted,
       blacklist_reason, created_at
  FROM customers
 WHERE deleted_at IS NULL
   AND (sqlc.narg(q)::text IS NULL
        OR name ILIKE '%' || sqlc.narg(q)::text || '%'
        OR phone ILIKE '%' || sqlc.narg(q)::text || '%')
   AND (sqlc.narg(cursor)::uuid IS NULL OR id < sqlc.narg(cursor)::uuid)
 ORDER BY id DESC
 LIMIT sqlc.arg(lim);

-- name: UpdateCustomer :execrows
-- Blacklisting is not here on purpose: an operator may call this, and BR-028
-- says an operator may not block anyone.
UPDATE customers SET
    name            = COALESCE(sqlc.narg(name),  name),
    phone           = COALESCE(sqlc.narg(phone), phone),
    id_type         = COALESCE(sqlc.narg(id_type), id_type),
    id_number_enc   = COALESCE(sqlc.narg(id_number_enc)::bytea, id_number_enc),
    id_number_last4 = COALESCE(sqlc.narg(id_number_last4), id_number_last4),
    updated_at      = now()
 WHERE id = sqlc.arg(id) AND deleted_at IS NULL;

-- name: SetCustomerBlacklist :execrows
-- One statement for both directions: customers_blacklist_has_reason makes the
-- flag and the reason travel together, so unblocking passes NULL for both.
UPDATE customers SET
    is_blacklisted   = sqlc.arg(is_blacklisted),
    blacklist_reason = sqlc.narg(blacklist_reason),
    updated_at       = now()
 WHERE id = sqlc.arg(id) AND deleted_at IS NULL;

-- name: InsertAuditLog :exec
-- The only statement against audit_logs. There is no UPDATE or DELETE query,
-- and 000012 revokes both from app_user so there cannot be one (BR-085).
INSERT INTO audit_logs (id, owner_id, actor_user_id, action, entity, entity_id, metadata)
VALUES ($1, $2, $3, $4, $5, $6, $7);
