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
       b.created_at,
       (b.status = 'picked_up' AND b.end_at < now())::boolean AS overdue,
       c.id AS customer_id, c.name AS customer_name, c.phone AS customer_phone,
       c.is_blacklisted AS customer_blacklisted,
       r.id AS resource_id, r.name AS resource_name,
       u.id AS unit_id, u.code AS unit_code, u.label AS unit_label
  FROM bookings b
  JOIN customers c      ON c.id = b.customer_id
  JOIN resources r      ON r.id = b.resource_id
  JOIN resource_units u ON u.id = b.resource_unit_id
 WHERE b.id = $1 AND b.deleted_at IS NULL;

-- name: ListBookings :many
-- from/to select bookings that OVERLAP the window, not ones that start in it.
-- overdue is derived, never stored (BR-041). Keyset on (start_at, id).
SELECT b.id, b.code, b.status, b.source, b.start_at, b.end_at, b.end_at_with_buffer,
       b.unit_price, b.pricing_unit, b.buffer_minutes, b.duration_qty, b.subtotal,
       b.deposit_amount, b.late_fee_per_unit, b.cancelled_reason, b.expires_at,
       b.created_at,
       (b.status = 'picked_up' AND b.end_at < now())::boolean AS overdue,
       c.id AS customer_id, c.name AS customer_name, c.phone AS customer_phone,
       c.is_blacklisted AS customer_blacklisted,
       r.id AS resource_id, r.name AS resource_name,
       u.id AS unit_id, u.code AS unit_code, u.label AS unit_label
  FROM bookings b
  JOIN customers c      ON c.id = b.customer_id
  JOIN resources r      ON r.id = b.resource_id
  JOIN resource_units u ON u.id = b.resource_unit_id
 WHERE b.deleted_at IS NULL
   AND (sqlc.narg(status)::text IS NULL OR b.status = sqlc.narg(status)::text)
   AND (sqlc.narg(from_at)::timestamptz IS NULL OR b.end_at > sqlc.narg(from_at)::timestamptz)
   AND (sqlc.narg(to_at)::timestamptz IS NULL OR b.start_at < sqlc.narg(to_at)::timestamptz)
   AND (sqlc.narg(unit_id)::uuid IS NULL OR b.resource_unit_id = sqlc.narg(unit_id)::uuid)
   AND (sqlc.narg(customer_id)::uuid IS NULL OR b.customer_id = sqlc.narg(customer_id)::uuid)
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
       b.status, c.name AS customer_name
  FROM bookings b
  JOIN customers c ON c.id = b.customer_id
 WHERE b.status IN ('reserved', 'picked_up') AND b.deleted_at IS NULL
   AND b.start_at < sqlc.arg(to_at)::timestamptz
   AND b.end_at_with_buffer > sqlc.arg(from_at)::timestamptz
 ORDER BY b.resource_unit_id, b.start_at;
