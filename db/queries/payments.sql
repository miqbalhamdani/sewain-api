-- Pembayaran dan bukti transfer.  (S1-044, S1-046)

-- name: LockInvoice :one
-- The row lock serialises two payments on one invoice; the unique index
-- payments_one_success_per_invoice is the backstop if anything ever skips it.
SELECT i.id, i.booking_id, i.status,
       COALESCE((SELECT sum(l.amount) FROM invoice_lines l WHERE l.invoice_id = i.id), 0)::bigint AS total
  FROM invoices i
 WHERE i.id = $1 AND i.deleted_at IS NULL
   FOR UPDATE OF i;

-- name: InsertPayment :exec
INSERT INTO payments (id, owner_id, invoice_id, method, amount, status, paid_at, approved_by)
VALUES ($1, $2, $3, $4, $5, 'success', $6, $7);

-- name: MarkInvoicePaid :exec
UPDATE invoices SET status = 'paid', paid_at = $2, updated_at = now() WHERE id = $1;

-- name: InsertProof :exec
INSERT INTO payment_proofs (id, owner_id, invoice_id, object_key, content_type, created_by)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: ListProofs :many
SELECT p.id, p.invoice_id, p.object_key, p.content_type, p.review_status, p.match_status,
       p.ai_amount, p.ai_paid_at, u.name AS reviewed_by_name, p.reviewed_at, p.reject_reason, p.created_at
  FROM payment_proofs p
  LEFT JOIN users u ON u.id = p.reviewed_by
 WHERE p.invoice_id = $1
 ORDER BY p.created_at DESC;

-- name: GetProof :one
SELECT p.id, p.invoice_id, p.object_key, p.content_type, p.review_status, p.match_status,
       p.ai_amount, p.ai_paid_at, u.name AS reviewed_by_name, p.reviewed_at, p.reject_reason, p.created_at
  FROM payment_proofs p
  LEFT JOIN users u ON u.id = p.reviewed_by
 WHERE p.id = $1;

-- name: LockProof :one
SELECT id, invoice_id, review_status FROM payment_proofs WHERE id = $1 FOR UPDATE;

-- name: ApproveProof :exec
UPDATE payment_proofs SET review_status = 'approved', reviewed_by = $2, reviewed_at = now(), payment_id = $3
 WHERE id = $1;

-- name: RejectProof :exec
UPDATE payment_proofs SET review_status = 'rejected', reviewed_by = $2, reviewed_at = now(), reject_reason = $3
 WHERE id = $1;

-- name: GetProofForScan :one
SELECT object_key, content_type, review_status, match_status FROM payment_proofs WHERE id = $1;

-- name: SetProofReading :execrows
-- Only on an unread, undecided proof: a second delivery of the same job (at-
-- least-once, BR-091) or a human who already decided both win over the reader.
UPDATE payment_proofs SET match_status = $2, ai_amount = $3, ai_paid_at = $4
 WHERE id = $1 AND match_status IS NULL AND review_status = 'pending';

-- name: ListOwnerInvoices :many
-- The owner's full list (S1-047), newest first, keyset on (created_at, id).
SELECT i.id, i.booking_id, i.number, i.status, i.due_at, i.paid_at, i.created_at,
       c.id AS customer_id, c.name AS customer_name, c.phone AS customer_phone,
       c.is_blacklisted AS customer_blacklisted,
       COALESCE((SELECT sum(l.amount) FROM invoice_lines l WHERE l.invoice_id = i.id), 0)::bigint AS total
  FROM invoices i
  LEFT JOIN customers c ON c.id = i.customer_id
 WHERE i.deleted_at IS NULL
   AND (sqlc.narg(status)::text IS NULL OR i.status = sqlc.narg(status)::text)
   AND (sqlc.narg(cursor_at)::timestamptz IS NULL
        OR (i.created_at, i.id) < (sqlc.narg(cursor_at)::timestamptz, sqlc.narg(cursor_id)::uuid))
 ORDER BY i.created_at DESC, i.id DESC
 LIMIT sqlc.arg(lim);

-- name: PendingProofInvoices :many
-- Invoices of one booking with a proof still waiting for a person (BR-062): the
-- portal says "menunggu dicek" instead of inviting a second upload.
SELECT DISTINCT p.invoice_id
  FROM payment_proofs p
  JOIN invoices i ON i.id = p.invoice_id
 WHERE i.booking_id = $1 AND p.review_status = 'pending';
