-- Invoice.  (S1-041)
--
-- No total column anywhere: every read sums the lines (BR-055).

-- name: InsertInvoice :exec
INSERT INTO invoices (id, owner_id, booking_id, customer_id, number, due_at, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: InsertInvoiceLine :exec
INSERT INTO invoice_lines (id, owner_id, invoice_id, kind, description, amount,
                           handover_photo_id, waiver_reason)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: CountBookingInvoices :one
-- The next invoice number of a booking is this + 1. Safe because every caller
-- holds the booking row lock (LockBooking*), and the unique index is the
-- backstop if one ever does not.
SELECT count(*) FROM invoices WHERE booking_id = $1;

-- name: GetPaymentDueHours :one
SELECT payment_due_hours FROM owners WHERE id = $1;

-- name: RentInvoiceState :one
-- BR-038 reads "the rent invoice is paid": the invoice carrying the rent line.
-- has_rent is false for a free booking (base_price 0), which has nothing to
-- pay and so nothing to block on.
SELECT EXISTS (SELECT 1 FROM invoices i
                 JOIN invoice_lines l ON l.invoice_id = i.id AND l.kind = 'rent'
                WHERE i.booking_id = $1 AND i.deleted_at IS NULL)::boolean AS has_rent,
       EXISTS (SELECT 1 FROM invoices i
                 JOIN invoice_lines l ON l.invoice_id = i.id AND l.kind = 'rent'
                WHERE i.booking_id = $1 AND i.status = 'paid' AND i.deleted_at IS NULL)::boolean AS paid;

-- name: ListBookingInvoices :many
SELECT i.id, i.booking_id, i.number, i.status, i.due_at, i.paid_at, i.created_at,
       c.id AS customer_id, c.name AS customer_name, c.phone AS customer_phone,
       c.is_blacklisted AS customer_blacklisted,
       COALESCE((SELECT sum(l.amount) FROM invoice_lines l WHERE l.invoice_id = i.id), 0)::bigint AS total
  FROM invoices i
  LEFT JOIN customers c ON c.id = i.customer_id
 WHERE i.booking_id = $1 AND i.deleted_at IS NULL
 ORDER BY i.created_at, i.number;

-- name: GetInvoice :one
SELECT i.id, i.booking_id, i.number, i.status, i.due_at, i.paid_at, i.created_at,
       c.id AS customer_id, c.name AS customer_name, c.phone AS customer_phone,
       c.is_blacklisted AS customer_blacklisted,
       COALESCE((SELECT sum(l.amount) FROM invoice_lines l WHERE l.invoice_id = i.id), 0)::bigint AS total
  FROM invoices i
  LEFT JOIN customers c ON c.id = i.customer_id
 WHERE i.id = $1 AND i.deleted_at IS NULL;

-- name: ListInvoiceLines :many
SELECT id, invoice_id, kind, description, amount, handover_photo_id, waiver_reason
  FROM invoice_lines
 WHERE invoice_id = ANY(sqlc.arg(invoice_ids)::uuid[])
 ORDER BY created_at, id;

-- name: CancelBookingInvoices :exec
-- Booking batal tidak menagih apa pun (BR-057 revisi): invoice yang belum
-- dibayar ikut batal; yang sudah paid tetap tercatat. Dipanggil Cancel manual,
-- dan nanti job payment_expired S1-052 lewat jalur yang sama.
UPDATE invoices SET status = 'cancelled', updated_at = now()
 WHERE booking_id = $1 AND status IN ('unpaid', 'gateway_pending', 'overdue')
   AND deleted_at IS NULL;
