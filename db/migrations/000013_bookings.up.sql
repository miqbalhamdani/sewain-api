-- Booking, dan constraint yang jadi klaim utama produk ini.  (S1-022, S1-024)
--
-- Transcribed from ../docs/03-erd.md sections 1 and 3, and proven ahead of time
-- by ../docs/03-verify-overlap-constraint.sql. Where this file and those
-- disagree, this file is the bug.

CREATE EXTENSION IF NOT EXISTS btree_gist;

CREATE TABLE bookings (
    id       uuid PRIMARY KEY,
    owner_id uuid NOT NULL REFERENCES owners(id),

    -- <prefix>-nnnn from booking_counters, never MAX()+1 (BR-024).
    code text NOT NULL,

    customer_id      uuid NOT NULL,
    resource_id      uuid NOT NULL,
    resource_unit_id uuid NOT NULL,

    start_at timestamptz NOT NULL,
    end_at   timestamptz NOT NULL,
    -- Filled by the trigger below. Never computed in Go, never a GENERATED
    -- column -- 03-erd.md section 3 says why, and the answer is PostgreSQL.
    end_at_with_buffer timestamptz NOT NULL,

    status text NOT NULL,
    source text NOT NULL DEFAULT 'staff',

    -- Snapshot at creation, never re-read from resources (BR-014, BR-015).
    -- NULL copies as NULL: no deposit, no late fee (BR-016).
    unit_price        bigint NOT NULL,
    pricing_unit      text   NOT NULL,
    buffer_minutes    int    NOT NULL DEFAULT 0,
    duration_qty      int    NOT NULL,
    subtotal          bigint NOT NULL,
    deposit_amount    bigint,
    late_fee_per_unit bigint,

    -- Deposit lifecycle (BR-048, BR-051). Nothing writes these until M3/M4.
    deposit_waived_at     timestamptz,
    deposit_waived_by     uuid REFERENCES users(id),
    deposit_waiver_reason text,
    actual_return_at      timestamptz,
    deposit_deducted      bigint NOT NULL DEFAULT 0,
    deposit_refunded      bigint NOT NULL DEFAULT 0,
    deposit_note          text,

    expires_at       timestamptz,   -- drafts only (BR-027)
    cancelled_reason text,

    created_by uuid REFERENCES users(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    deleted_at timestamptz
);

-- BR-015 + BR-022: the buffer is the DATABASE's arithmetic.
CREATE FUNCTION bookings_fill_end_at_with_buffer()
RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  NEW.end_at_with_buffer := NEW.end_at + make_interval(mins => NEW.buffer_minutes);
  RETURN NEW;
END $fn$;

-- No column list, and BEFORE UPDATE too: the trigger always overwrites, so a
-- client value for the column is replaced rather than trusted.
CREATE TRIGGER bookings_end_at_with_buffer
    BEFORE INSERT OR UPDATE ON bookings
    FOR EACH ROW EXECUTE FUNCTION bookings_fill_end_at_with_buffer();

-- BR-022: anti double-booking. The only source of truth -- no app lock, no
-- advisory lock, no queue. S1-023 is the test that keeps it that way.
--
-- 03-erd.md used to list a second gist index identical to this one, which only
-- doubled the cost of every INSERT. Reads use bookings_unit_start below.
ALTER TABLE bookings
    ADD CONSTRAINT bookings_no_overlap
    EXCLUDE USING gist (
        resource_unit_id WITH =,
        tstzrange(start_at, end_at_with_buffer, '[)') WITH &&
    )
    WHERE (status IN ('reserved', 'picked_up') AND deleted_at IS NULL);

ALTER TABLE bookings
    ADD CONSTRAINT bookings_range_valid  CHECK (end_at > start_at),
    ADD CONSTRAINT bookings_buffer_valid CHECK (end_at_with_buffer >= end_at),
    ADD CONSTRAINT bookings_status_valid
        CHECK (status IN ('draft', 'reserved', 'picked_up', 'returned',
                          'completed', 'cancelled', 'no_show')),
    ADD CONSTRAINT bookings_source_valid
        CHECK (source IN ('staff', 'public_page')),
    ADD CONSTRAINT bookings_cancelled_reason_valid
        CHECK ((status = 'cancelled') = (cancelled_reason IS NOT NULL)
               AND (cancelled_reason IS NULL
                    OR cancelled_reason IN ('manual', 'expired', 'payment_expired'))),
    ADD CONSTRAINT bookings_snapshot_valid
        CHECK (unit_price >= 0 AND duration_qty > 0 AND subtotal >= 0
               AND (deposit_amount    IS NULL OR deposit_amount    > 0)
               AND (late_fee_per_unit IS NULL OR late_fee_per_unit > 0)),
    ADD CONSTRAINT bookings_deposit_nonneg
        CHECK (deposit_deducted >= 0 AND deposit_refunded >= 0
               AND (deposit_amount IS NOT NULL
                    OR (deposit_deducted = 0 AND deposit_refunded = 0))
               AND (deposit_amount IS NULL OR deposit_deducted <= deposit_amount)),
    ADD CONSTRAINT bookings_deposit_waiver_complete
        CHECK (num_nonnulls(deposit_waived_at, deposit_waived_by, deposit_waiver_reason)
               IN (0, 3));

-- BR-001 + BR-029: one three-column key says both "this unit is this rental's"
-- and "this unit is of this booking's resource". A swap to another kind of
-- thing is therefore refused here, not just checked in Go -- the booking's
-- price snapshot belongs to the original resource.
ALTER TABLE resource_units ADD CONSTRAINT resource_units_id_resource_owner_uq
    UNIQUE (id, resource_id, owner_id);
ALTER TABLE bookings ADD CONSTRAINT bookings_unit_matches_resource
    FOREIGN KEY (resource_unit_id, resource_id, owner_id)
    REFERENCES resource_units (id, resource_id, owner_id);
ALTER TABLE bookings ADD CONSTRAINT bookings_customer_matches_owner
    FOREIGN KEY (customer_id, owner_id) REFERENCES customers (id, owner_id);

-- BR-024: unique per owner, never reused.
CREATE UNIQUE INDEX bookings_code_per_owner ON bookings (owner_id, code);

-- Availability, the conflict pre-check and the calendar. NOT the gist index of
-- bookings_no_overlap, though it covers the same rows: under RLS, PostgreSQL
-- will not use a qual that is not leakproof as an index condition, and range
-- && / tstzrange() are not. Every read here therefore writes the overlap as two
-- plain timestamptz comparisons, which are leakproof, against this btree.
-- Measured: 1.6 s -> milliseconds for 500 units x 12 months (S1-025). The
-- constraint itself is unaffected -- it is checked as the table owner.
CREATE INDEX bookings_unit_start ON bookings (owner_id, resource_unit_id, start_at)
    WHERE status IN ('reserved', 'picked_up') AND deleted_at IS NULL;

-- The list screen's default order and the status filters.
CREATE INDEX bookings_owner_status_start ON bookings (owner_id, status, start_at);
CREATE INDEX bookings_owner_start ON bookings (owner_id, start_at DESC, id DESC)
    WHERE deleted_at IS NULL;

SELECT enable_owner_rls('bookings');

-- ---------------------------------------------------------------------------
-- booking_counters -- BR-024
-- ---------------------------------------------------------------------------

CREATE TABLE booking_counters (
    owner_id    uuid PRIMARY KEY REFERENCES owners(id),
    last_number bigint NOT NULL DEFAULT 0   -- only ever goes up
);

SELECT enable_owner_rls('booking_counters');
