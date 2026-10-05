-- Laporan dan dashboard.  (S1-056, S1-063)
--
-- Every query runs inside InOwnerTx; RLS scopes it to one rental. Ranges are
-- written as plain timestamptz comparisons, never tstzrange && -- under RLS
-- the range form cannot use an index (000013).

-- name: RevenuePaidLines :many
-- Cash basis (BR-076): lines of invoices paid inside the period.
SELECT l.kind, COALESCE(sum(l.amount), 0)::bigint AS amount
  FROM invoice_lines l
  JOIN invoices i ON i.id = l.invoice_id
 WHERE i.status = 'paid' AND i.deleted_at IS NULL
   AND i.paid_at >= sqlc.arg(from_at)::timestamptz AND i.paid_at < sqlc.arg(to_at)::timestamptz
 GROUP BY l.kind;

-- name: AbsorbedCharges :many
-- Charges a deposit absorbed at settlement (M4): the late_fee and damage lines
-- of a settled booking's cancelled invoices, in the order settle consumed
-- them. A returned booking is never cancelled, so its cancelled invoices are
-- exactly the absorbed ones.
SELECT b.id AS booking_id, b.deposit_deducted, l.kind, l.amount
  FROM bookings b
  JOIN invoices i      ON i.booking_id = b.id AND i.status = 'cancelled' AND i.deleted_at IS NULL
  JOIN invoice_lines l ON l.invoice_id = i.id AND l.kind IN ('late_fee', 'damage')
 WHERE b.deposit_settled_at >= sqlc.arg(from_at)::timestamptz
   AND b.deposit_settled_at < sqlc.arg(to_at)::timestamptz
   AND b.deposit_deducted > 0 AND b.deleted_at IS NULL
 ORDER BY b.id, l.created_at, l.id;

-- name: DepositsReturned :one
SELECT COALESCE(sum(deposit_refunded), 0)::bigint
  FROM bookings
 WHERE deposit_settled_at >= sqlc.arg(from_at)::timestamptz
   AND deposit_settled_at < sqlc.arg(to_at)::timestamptz AND deleted_at IS NULL;

-- name: DepositBalance :one
-- Held right now: deposits paid, neither settled nor waived (BR-050).
SELECT COALESCE(sum(l.amount), 0)::bigint
  FROM invoice_lines l
  JOIN invoices i ON i.id = l.invoice_id AND i.status = 'paid' AND i.deleted_at IS NULL
  JOIN bookings b ON b.id = i.booking_id
 WHERE l.kind = 'deposit' AND b.deposit_settled_at IS NULL AND b.deposit_waived_at IS NULL
   AND b.deleted_at IS NULL;

-- name: UnitUtilization :many
-- Seconds of each active unit covered by a booking that actually went out.
SELECT u.id, u.code, u.label, r.name AS resource_name,
       COALESCE(sum(EXTRACT(EPOCH FROM
         LEAST(COALESCE(b.actual_return_at, b.end_at), sqlc.arg(to_at)::timestamptz)
         - GREATEST(b.start_at, sqlc.arg(from_at)::timestamptz)))
         -- FILTER: LEAST/GREATEST skip NULLs, so a unit with no booking would
         -- otherwise count as rented for the whole period.
         FILTER (WHERE b.id IS NOT NULL), 0)::float8 AS rented_seconds
  FROM resource_units u
  JOIN resources r ON r.id = u.resource_id AND r.deleted_at IS NULL
  LEFT JOIN bookings b ON b.resource_unit_id = u.id AND b.deleted_at IS NULL
        AND b.status IN ('picked_up', 'returned', 'completed')
        AND b.start_at < sqlc.arg(to_at)::timestamptz
        AND COALESCE(b.actual_return_at, b.end_at) > sqlc.arg(from_at)::timestamptz
 WHERE u.status = 'active' AND u.deleted_at IS NULL
 GROUP BY u.id, u.code, u.label, r.name
 ORDER BY rented_seconds, u.code;

-- name: IdleUnits :many
-- Active units whose last rental ended before the cutoff -- or that were never
-- rented and have existed since before it.
SELECT u.id, u.code, u.label, r.name AS resource_name,
       (max(COALESCE(b.actual_return_at, b.end_at)) IS NOT NULL)::boolean AS ever_rented,
       COALESCE(max(COALESCE(b.actual_return_at, b.end_at)), u.created_at)::timestamptz AS idle_since
  FROM resource_units u
  JOIN resources r ON r.id = u.resource_id AND r.deleted_at IS NULL
  LEFT JOIN bookings b ON b.resource_unit_id = u.id AND b.deleted_at IS NULL
        AND b.status IN ('picked_up', 'returned', 'completed')
 WHERE u.status = 'active' AND u.deleted_at IS NULL
 GROUP BY u.id, u.code, u.label, r.name, u.created_at
HAVING COALESCE(max(COALESCE(b.actual_return_at, b.end_at)), u.created_at) < sqlc.arg(cutoff)::timestamptz
   -- COALESCE: bool_or over a unit with no bookings is NULL, and NOT NULL
   -- would quietly drop every never-rented unit from the list.
   AND NOT COALESCE(bool_or(b.status = 'picked_up'), false)
 ORDER BY idle_since, u.code;
