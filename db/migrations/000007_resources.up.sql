-- Catalogue, part one: resources -- the kind of thing, not the thing.  (S1-014)
--
-- Transcribed from ../docs/03-erd.md sections 1 and 3. Where this file and those
-- sections disagree, this file is the bug.
--
-- BR-010 splits the catalogue in two, and the split is the whole model: three
-- Avanzas are ONE resource and THREE resource_units, because a booking has to
-- name the physical car that leaves the yard. Pricing, deposit and buffer live
-- up here on the kind; identity (plate, serial) lives down there on the thing.
--
-- Deliberately NOT here:
--   `description` -- PRD section 6.2 has it, 03-erd.md section 1 does not, and
--     nothing renders it. A free-text column no screen shows is a column that
--     gets filled once and then forgotten. Recorded in 03-erd.md section 5.
--   `is_metered` -- 04-api-spec.md requires an odometer at pickup "for metered
--     resources" but nothing anywhere marks one, and `meter_value` is a column
--     on the UNITS. A real contract gap, and it belongs to S1-035 rather than
--     to a guess made here.

CREATE TABLE resources (
    id       uuid PRIMARY KEY,
    owner_id uuid NOT NULL REFERENCES owners(id),

    name     text NOT NULL,
    category text,

    -- Object keys, never URLs -- endpoint and region change, keys do not.
    -- Nothing writes this until S1-033 presigns the upload. It is created now
    -- because 03-erd.md section 1 fixes the shape of the table, and migrations
    -- are forward-only: an ALTER later costs more than a column today.
    images text[] NOT NULL DEFAULT '{}',

    -- BR-012 + BR-017: filled by the SERVER from owners.business_type when the
    -- row is created. The client cannot send it -- it is not in ResourceCreate,
    -- so the generated code has no field for it. The default and the CHECK are
    -- the second line of defence: without them an INSERT that forgets it yields
    -- NULL, and 'bulan' is accepted. Both produce a wrong invoice rather than
    -- merely dirty data, because the unit is used twice in the money maths
    -- (duration_qty, and the late-fee formula in BR-046).
    pricing_unit text NOT NULL DEFAULT 'day',

    -- Money is bigint, full rupiah. 350000 is Rp 350.000 -- not cents, not
    -- float. This differs from new-commerce; do not copy its helpers.
    base_price bigint NOT NULL,

    -- BR-016: the four that may be empty, where empty means THE RULE DOES NOT
    -- APPLY. NULL and 0 are different states and must never be interchangeable:
    -- NULL is "no deposit at all", 0 would be "a deposit of nothing". No rental
    -- needs the second, so the CHECKs below refuse it -- one state, one way to
    -- write it. Without that, two insert paths produce two values for the same
    -- intent and no report can tell them apart again.
    deposit_amount    bigint,
    late_fee_per_unit bigint,
    min_duration      int,
    max_duration      int,

    -- BR-015: the one optional field that may NOT be empty, and the reason is
    -- arithmetic rather than taste. `end_at + NULL` is NULL, and
    -- tstzrange(start_at, NULL) is unbounded above -- that unit would collide
    -- with every future booking it has. "No gap" is already exactly 0.
    buffer_minutes int NOT NULL DEFAULT 0,

    -- Not nullable either, for a smaller reason: a NULL boolean is a third
    -- state that means nothing to anyone. "Not required" is exactly false.
    requires_id_verification boolean NOT NULL DEFAULT false,

    -- Named below rather than inline: an inline CHECK gets whatever name
    -- PostgreSQL invents (resources_status_check), and internal/catalog
    -- matches on names. A translator keyed on a name nobody wrote is a 500
    -- where the contract promised a 422 -- which is exactly how this was
    -- found, by the test rather than by reading.
    status text NOT NULL DEFAULT 'active',

    -- 03-erd.md section 2 wants created_by on every table. owners and users
    -- predate that rule and do not have it; new tables do.
    created_by uuid REFERENCES users(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    deleted_at timestamptz
);

-- Named separately rather than inline, because internal/catalog matches on
-- these names to turn a refusal into the error code the contract promises.
-- Constraint names are ours; PostgreSQL's messages are PostgreSQL's to change.
ALTER TABLE resources
    ADD CONSTRAINT resources_deposit_positive
        CHECK (deposit_amount    IS NULL OR deposit_amount    > 0),
    ADD CONSTRAINT resources_late_fee_positive
        CHECK (late_fee_per_unit IS NULL OR late_fee_per_unit > 0),
    ADD CONSTRAINT resources_min_duration_positive
        CHECK (min_duration      IS NULL OR min_duration      > 0),
    ADD CONSTRAINT resources_max_duration_positive
        CHECK (max_duration      IS NULL OR max_duration      > 0),
    ADD CONSTRAINT resources_duration_order
        CHECK (min_duration IS NULL OR max_duration IS NULL
               OR max_duration >= min_duration),
    ADD CONSTRAINT resources_buffer_nonneg CHECK (buffer_minutes >= 0),
    -- Not in 03-erd.md section 3, and added anyway: a negative base price is a
    -- negative invoice, which is the class of thing every other constraint in
    -- this file exists to make unrepresentable. Zero is allowed -- a charger
    -- thrown in with the camera is a real rental, and unlike the four above
    -- this column is NOT NULL, so 0 has no NULL to be confused with.
    ADD CONSTRAINT resources_base_price_nonneg CHECK (base_price >= 0),
    ADD CONSTRAINT resources_pricing_unit_valid
        CHECK (pricing_unit IN ('hour','day','week','month')),
    ADD CONSTRAINT resources_status_valid
        CHECK (status IN ('active','inactive'));

-- Target for the composite foreign key in 000008. id alone is already the
-- primary key; this exists so (resource_id, owner_id) has something to point
-- at. Same shape, and the same reason, as users_id_owner_uq in 000002.
ALTER TABLE resources ADD CONSTRAINT resources_id_owner_uq UNIQUE (id, owner_id);

-- Composite and owner_id first: RLS adds that predicate to every query, so an
-- index that cannot serve it is dead weight.
CREATE INDEX resources_owner_status ON resources (owner_id, status)
    WHERE deleted_at IS NULL;

SELECT enable_owner_rls('resources');
