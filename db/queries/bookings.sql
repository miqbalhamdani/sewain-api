-- Booking, ketersediaan, kalender.  (S1-022 .. S1-027, S1-078)
--
-- Every query runs inside InOwnerTx; RLS scopes the rows. owner_id appears only
-- as an INSERT value. Nothing here prevents a double booking -- that is
-- bookings_no_overlap's job alone (BR-022). The conflict query below only makes
-- the refusal readable.

-- name: NextBookingNumber :one
-- BR-024: never MAX(code)+1 -- two concurrent creates would read the same max.
-- The upsert takes the row lock, so the second waits for the first and gets
-- the next number. A deleted booking's number is never handed out again.
INSERT INTO booking_counters (owner_id, last_number) VALUES ($1, 1)
ON CONFLICT (owner_id) DO UPDATE
    SET last_number = booking_counters.last_number + 1
RETURNING last_number;

-- name: GetBookingCodePrefix :one
-- owners has no RLS, hence the explicit WHERE. Read at the moment the code is
-- made: changing the prefix later never rewrites an old code (BR-024).
SELECT booking_code_prefix FROM owners WHERE id = $1;

-- name: GetBookableUnit :one
-- Everything a booking snapshots, read in the same transaction it is written
-- in (BR-014, BR-015, BR-016).
SELECT u.id AS unit_id, u.resource_id, u.status AS unit_status,
       r.status AS resource_status, r.base_price, r.pricing_unit, r.buffer_minutes,
       r.deposit_amount, r.late_fee_per_unit, r.min_duration, r.max_duration
  FROM resource_units u
  JOIN resources r ON r.id = u.resource_id
 WHERE u.id = $1 AND u.deleted_at IS NULL AND r.deleted_at IS NULL;

-- name: GetCustomerBlacklisted :one
SELECT is_blacklisted FROM customers WHERE id = $1 AND deleted_at IS NULL;

-- name: FindConflicts :many
-- The same overlap bookings_no_overlap tests, so the readable answer and the
-- constraint's answer agree: both ranges carry their own buffer. Written as two
-- comparisons, not tstzrange &&, so RLS lets it use an index -- see
-- bookings_unit_start in 000013. [a, b) and [c, d) overlap iff a < d AND c < b. Used before
-- writing (for the message) and after losing the race (for the list).
SELECT code, start_at, end_at, status
  FROM bookings
 WHERE resource_unit_id = sqlc.arg(unit_id)
   AND id <> sqlc.arg(exclude_id)
   AND status IN ('reserved', 'picked_up') AND deleted_at IS NULL
   AND start_at < sqlc.arg(end_at)::timestamptz
                  + make_interval(mins => sqlc.arg(buffer_minutes)::int)
   AND end_at_with_buffer > sqlc.arg(start_at)::timestamptz
 ORDER BY start_at;

-- name: InsertBooking :exec
-- end_at_with_buffer is not in the column list: the BEFORE trigger fills it,
-- and NOT NULL is checked after BEFORE triggers run.
INSERT INTO bookings (id, owner_id, created_by, code, customer_id, resource_id,
                      resource_unit_id, start_at, end_at,
                      status, source, unit_price, pricing_unit, buffer_minutes,
                      duration_qty, subtotal, deposit_amount, late_fee_per_unit)
VALUES (sqlc.arg(id), sqlc.arg(owner_id), sqlc.arg(created_by), sqlc.arg(code),
        sqlc.arg(customer_id), sqlc.arg(resource_id), sqlc.arg(resource_unit_id),
        sqlc.arg(start_at), sqlc.arg(end_at),
        sqlc.arg(status), sqlc.arg(source), sqlc.arg(unit_price),
        sqlc.arg(pricing_unit), sqlc.arg(buffer_minutes), sqlc.arg(duration_qty),
        sqlc.arg(subtotal), sqlc.narg(deposit_amount), sqlc.narg(late_fee_per_unit));

-- name: GetBooking :one
SELECT b.id, b.code, b.status, b.source, b.start_at, b.end_at, b.end_at_with_buffer,
       b.unit_price, b.pricing_unit, b.buffer_minutes, b.duration_qty, b.subtotal,
       b.deposit_amount, b.late_fee_per_unit, b.cancelled_reason, b.expires_at,
       b.created_at, b.actual_return_at, b.deposit_waived_at, b.deposit_settled_at,
       b.deposit_deducted, b.deposit_refunded,
       (b.status = 'picked_up' AND b.end_at < now())::boolean AS overdue,
       c.id AS customer_id, c.name AS customer_name, c.phone AS customer_phone,
       c.is_blacklisted AS customer_blacklisted,
       r.id AS resource_id, r.name AS resource_name,
       u.id AS unit_id, u.code AS unit_code, u.label AS unit_label,
       pay.n_active, pay.n_overdue, pay.n_unpaid, pay.outstanding
  FROM bookings b
  JOIN customers c      ON c.id = b.customer_id
  JOIN resources r      ON r.id = b.resource_id
  JOIN resource_units u ON u.id = b.resource_unit_id
  -- Ringkasan bayar, dihitung saat dibaca seperti kolom overdue (ide
  -- booking-invoice-lists): cancelled diabaikan, gateway_pending = belum bayar.
  -- ponytail: sum baris per invoice di dalam lateral tanpa index baru --
  -- invoices_owner_booking menjangkarnya; ANALYZE kalau perf test protes.
  CROSS JOIN LATERAL (
    SELECT count(*)::int AS n_active,
           (count(*) FILTER (WHERE i.status = 'overdue'))::int AS n_overdue,
           (count(*) FILTER (WHERE i.status IN ('unpaid', 'gateway_pending')))::int AS n_unpaid,
           COALESCE(sum((SELECT sum(l.amount) FROM invoice_lines l WHERE l.invoice_id = i.id))
                      FILTER (WHERE i.status IN ('unpaid', 'gateway_pending', 'overdue')), 0)::bigint AS outstanding
      FROM invoices i
     WHERE i.booking_id = b.id AND i.status <> 'cancelled' AND i.deleted_at IS NULL
  ) pay
 WHERE b.id = $1 AND b.deleted_at IS NULL;

-- name: ListBookings :many
-- from/to select bookings that OVERLAP the window, not ones that start in it.
-- overdue is derived, never stored (BR-041). Keyset on (start_at, id).
-- ponytail: code is a substring scan with no index -- fine inside one
-- owner's RLS slice; add pg_trgm if a fleet ever makes this slow.
SELECT b.id, b.code, b.status, b.source, b.start_at, b.end_at, b.end_at_with_buffer,
       b.unit_price, b.pricing_unit, b.buffer_minutes, b.duration_qty, b.subtotal,
       b.deposit_amount, b.late_fee_per_unit, b.cancelled_reason, b.expires_at,
       b.created_at, b.actual_return_at, b.deposit_waived_at, b.deposit_settled_at,
       b.deposit_deducted, b.deposit_refunded,
       (b.status = 'picked_up' AND b.end_at < now())::boolean AS overdue,
       c.id AS customer_id, c.name AS customer_name, c.phone AS customer_phone,
       c.is_blacklisted AS customer_blacklisted,
       r.id AS resource_id, r.name AS resource_name,
       u.id AS unit_id, u.code AS unit_code, u.label AS unit_label,
       pay.n_active, pay.n_overdue, pay.n_unpaid, pay.outstanding
  FROM bookings b
  JOIN customers c      ON c.id = b.customer_id
  JOIN resources r      ON r.id = b.resource_id
  JOIN resource_units u ON u.id = b.resource_unit_id
  -- Ringkasan bayar, dihitung saat dibaca seperti kolom overdue (ide
  -- booking-invoice-lists): cancelled diabaikan, gateway_pending = belum bayar.
  -- ponytail: sum baris per invoice di dalam lateral tanpa index baru --
  -- invoices_owner_booking menjangkarnya; ANALYZE kalau perf test protes.
  CROSS JOIN LATERAL (
    SELECT count(*)::int AS n_active,
           (count(*) FILTER (WHERE i.status = 'overdue'))::int AS n_overdue,
           (count(*) FILTER (WHERE i.status IN ('unpaid', 'gateway_pending')))::int AS n_unpaid,
           COALESCE(sum((SELECT sum(l.amount) FROM invoice_lines l WHERE l.invoice_id = i.id))
                      FILTER (WHERE i.status IN ('unpaid', 'gateway_pending', 'overdue')), 0)::bigint AS outstanding
      FROM invoices i
     WHERE i.booking_id = b.id AND i.status <> 'cancelled' AND i.deleted_at IS NULL
  ) pay
 WHERE b.deleted_at IS NULL
   AND (sqlc.narg(status)::text IS NULL OR b.status = sqlc.narg(status)::text)
   AND (sqlc.narg(from_at)::timestamptz IS NULL OR b.end_at > sqlc.narg(from_at)::timestamptz)
   AND (sqlc.narg(to_at)::timestamptz IS NULL OR b.start_at < sqlc.narg(to_at)::timestamptz)
   AND (sqlc.narg(unit_ids)::uuid[] IS NULL OR b.resource_unit_id = ANY(sqlc.narg(unit_ids)::uuid[]))
   AND (sqlc.narg(customer_ids)::uuid[] IS NULL OR b.customer_id = ANY(sqlc.narg(customer_ids)::uuid[]))
   AND (sqlc.narg(resource_ids)::uuid[] IS NULL OR b.resource_id = ANY(sqlc.narg(resource_ids)::uuid[]))
   -- strpos, not ILIKE: a '%' or '_' the user typed is a character, not a wildcard.
   AND (sqlc.narg(code)::text IS NULL OR strpos(lower(b.code), lower(sqlc.narg(code)::text)) > 0)
   AND (NOT sqlc.arg(overdue_only)::boolean
        OR (b.status = 'picked_up' AND b.end_at < now()))
   AND (sqlc.narg(cursor_start)::timestamptz IS NULL
        OR (b.start_at, b.id) < (sqlc.narg(cursor_start)::timestamptz, sqlc.narg(cursor_id)::uuid))
 ORDER BY b.start_at DESC, b.id DESC
 LIMIT sqlc.arg(lim);

-- name: LockBooking :one
-- FOR UPDATE so a confirm, a cancel and a swap on the same booking serialise
-- instead of each deciding from a status the other is about to change.
SELECT status, resource_unit_id, start_at, end_at, buffer_minutes
  FROM bookings
 WHERE id = $1 AND deleted_at IS NULL
   FOR UPDATE;

-- name: ConfirmBooking :exec
UPDATE bookings SET status = 'reserved', expires_at = NULL, updated_at = now()
 WHERE id = $1;

-- name: CancelBooking :exec
UPDATE bookings SET status = 'cancelled', cancelled_reason = 'manual', updated_at = now()
 WHERE id = $1;

-- name: SwapBookingUnit :exec
-- resource_id is untouched: bookings_unit_matches_resource refuses a unit of
-- another kind, and the price snapshot stays the original resource's (BR-029).
UPDATE bookings SET resource_unit_id = sqlc.arg(unit_id), updated_at = now()
 WHERE id = sqlc.arg(id);

-- name: ListBookableResources :many
SELECT id, name, base_price, pricing_unit, deposit_amount, buffer_minutes
  FROM resources
 WHERE status = 'active' AND deleted_at IS NULL
   AND (sqlc.narg(resource_id)::uuid IS NULL OR id = sqlc.narg(resource_id)::uuid)
 ORDER BY name;

-- name: ListAvailableUnits :many
-- BR-020, with one deliberate widening: the requested range carries the
-- resource's own buffer, exactly as the booking would once written. Without it
-- a unit could be offered here and then refused by bookings_no_overlap -- the
-- screen and the constraint must give the same answer.
SELECT u.id, u.resource_id, u.code, u.label
  FROM resource_units u
  JOIN resources r ON r.id = u.resource_id
 WHERE u.status = 'active' AND u.deleted_at IS NULL
   AND r.status = 'active' AND r.deleted_at IS NULL
   AND (sqlc.narg(resource_id)::uuid IS NULL OR u.resource_id = sqlc.narg(resource_id)::uuid)
   AND NOT EXISTS (
       SELECT 1 FROM bookings b
        WHERE b.resource_unit_id = u.id
          AND b.status IN ('reserved', 'picked_up') AND b.deleted_at IS NULL
          AND b.start_at < sqlc.arg(end_at)::timestamptz + make_interval(mins => r.buffer_minutes)
          AND b.end_at_with_buffer > sqlc.arg(start_at)::timestamptz)
 ORDER BY u.code;

-- name: ListCalendarUnits :many
-- Every lane except retired (BR-033). Maintenance keeps its lane.
SELECT u.id, u.resource_id, u.code, u.label, u.status
  FROM resource_units u
  JOIN resources r ON r.id = u.resource_id
 WHERE u.deleted_at IS NULL AND u.status <> 'retired' AND r.deleted_at IS NULL
 ORDER BY r.name, u.code;

-- name: ListCalendarBookings :many
-- Only the two statuses that lock (BR-023): draft never appears, returned shows
-- as available.
SELECT b.id, b.code, b.resource_unit_id, b.start_at, b.end_at, b.end_at_with_buffer,
       b.status, c.name AS customer_name,
       -- reserved_paid vs reserved_unpaid (BR-033): the rent invoice's status.
       EXISTS (SELECT 1 FROM invoices i
                 JOIN invoice_lines l ON l.invoice_id = i.id AND l.kind = 'rent'
                WHERE i.booking_id = b.id AND i.status = 'paid')::boolean AS rent_paid
  FROM bookings b
  JOIN customers c ON c.id = b.customer_id
 WHERE b.status IN ('reserved', 'picked_up') AND b.deleted_at IS NULL
   AND b.start_at < sqlc.arg(to_at)::timestamptz
   AND b.end_at_with_buffer > sqlc.arg(from_at)::timestamptz
 ORDER BY b.resource_unit_id, b.start_at;

-- name: LockBookingForHandover :one
-- Everything pickup and return decide from, read under the row lock.
-- is_vehicle is the "metered" rule (S1-035): a resource with vehicle specs.
SELECT b.status, b.code, b.customer_id, b.resource_unit_id, b.start_at, b.end_at,
       b.pricing_unit, b.late_fee_per_unit, b.deposit_amount,
       EXISTS (SELECT 1 FROM vehicle_specs v WHERE v.resource_id = b.resource_id)::boolean AS is_vehicle,
       o.require_payment_before_pickup
  FROM bookings b
  JOIN owners o ON o.id = b.owner_id
 WHERE b.id = $1 AND b.deleted_at IS NULL
   FOR UPDATE OF b;

-- name: FindPhysicalConflicts :many
-- BR-042: the unit is still out with an earlier booking past its end. The
-- schedule does not clash -- bookings_no_overlap compared ranges that have
-- already ended -- but the car is not in the yard. Plain timestamptz
-- comparison, not a range, for the RLS reason in 000013.
SELECT code, start_at, end_at, status
  FROM bookings
 WHERE resource_unit_id = sqlc.arg(unit_id) AND id <> sqlc.arg(exclude_id)
   AND status = 'picked_up' AND end_at < now() AND deleted_at IS NULL
 ORDER BY start_at;

-- name: MarkPickedUp :exec
UPDATE bookings SET status = 'picked_up', updated_at = now() WHERE id = $1;

-- name: MarkReturned :exec
-- BR-040: the server's clock, never the body's -- the same instant the late
-- fee was computed from, passed in so the two cannot disagree.
UPDATE bookings SET status = 'returned', actual_return_at = $2, updated_at = now()
 WHERE id = $1;

-- name: SetUnitMeter :exec
-- The handover is where the odometer gets read; the unit keeps the latest.
UPDATE resource_units SET meter_value = $2, updated_at = now() WHERE id = $1;

-- name: LockBookingDeposit :one
SELECT status, code, customer_id, deposit_amount, deposit_waived_at, deposit_settled_at
  FROM bookings
 WHERE id = $1 AND deleted_at IS NULL
   FOR UPDATE;

-- name: DepositInvoice :one
-- The invoice carrying the deposit line, and whether it is paid -- "the money
-- is in the owner's hands" (BR-048) or "can still be waived" (BR-051).
SELECT i.id, i.status
  FROM invoices i
  JOIN invoice_lines l ON l.invoice_id = i.id AND l.kind = 'deposit'
 WHERE i.booking_id = $1 AND i.deleted_at IS NULL
 LIMIT 1;

-- name: ReturnCharges :many
-- What the deposit absorbs at settlement: late fee and damage lines issued at
-- return, on invoices not yet paid. A return invoice the renter already paid
-- is settled money; absorbing it again is the double charge settle exists to
-- prevent (BR-048).
SELECT l.invoice_id, l.kind, l.description, l.amount, l.handover_photo_id
  FROM invoice_lines l
  JOIN invoices i ON i.id = l.invoice_id
 WHERE i.booking_id = $1 AND i.deleted_at IS NULL AND i.status IN ('unpaid', 'overdue')
   AND l.kind IN ('late_fee', 'damage')
 ORDER BY l.created_at, l.id;

-- name: CancelInvoice :exec
UPDATE invoices SET status = 'cancelled', updated_at = now() WHERE id = $1;

-- name: SettleDeposit :exec
UPDATE bookings SET deposit_deducted = $2, deposit_refunded = $3, deposit_note = $4,
       deposit_settled_at = now(), updated_at = now()
 WHERE id = $1;

-- name: CompleteBooking :exec
UPDATE bookings SET status = 'completed', updated_at = now() WHERE id = $1;

-- name: DeleteDepositLine :execrows
-- BR-051: the line is removed, not offset with a discount -- a discount counts
-- as revenue (BR-076), and offsetting would shrink revenue by the deposit.
DELETE FROM invoice_lines WHERE invoice_id = $1 AND kind = 'deposit';

-- name: WaiveDeposit :exec
UPDATE bookings SET deposit_waived_at = now(), deposit_waived_by = $2, deposit_waiver_reason = $3,
       updated_at = now()
 WHERE id = $1;
