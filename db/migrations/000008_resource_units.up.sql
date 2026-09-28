-- Catalogue, part two: resource_units -- the physical thing.  (S1-015)
--
-- Transcribed from ../docs/03-erd.md sections 1 and 3. Where this file and those
-- sections disagree, this file is the bug.
--
-- This is the row a booking points at, and the row bookings_no_overlap keys on
-- (S1-022). Everything M2 builds hangs off `id` here.

CREATE TABLE resource_units (
    id          uuid PRIMARY KEY,
    owner_id    uuid NOT NULL REFERENCES owners(id),
    resource_id uuid NOT NULL,

    -- Plate or serial number: what the person at the counter uses to tell one
    -- car from the next (BR-011). Unique per rental, not globally -- two
    -- rentals may both have B 1234 XY, and the partial index below says so.
    --
    -- Not to be confused with bookings.code, which IS server-generated and IS
    -- on CLAUDE.md's server-managed list. This one is typed by a human.
    code  text NOT NULL,
    label text,

    -- BR-013. Only 'active' appears in availability search; 'maintenance' still
    -- gets a calendar lane, 'retired' gets none.
    --
    -- There is no 'rented' here on purpose: "currently out" is derived from a
    -- booking in picked_up (BR-023), and 03-erd.md section 4 lists it among the
    -- columns that must not be added. Two sources for one fact means one of
    -- them is wrong, and it is always the cached one.
    -- Named in an ALTER below, not inline: internal/catalog matches on
    -- constraint names, and an inline CHECK is named by PostgreSQL.
    status text NOT NULL DEFAULT 'active',

    meter_value     bigint,
    condition_notes text,

    created_by uuid REFERENCES users(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    deleted_at timestamptz
);

ALTER TABLE resource_units
    ADD CONSTRAINT resource_units_status_valid
        CHECK (status IN ('active','maintenance','retired'));

-- BR-001: a unit CANNOT point at another rental's resource.
--
-- RLS does not close this one. Foreign key checks run as the table owner and
-- bypass row level security entirely, so a plain REFERENCES resources(id)
-- accepts any id in the table, including one this tenant can neither see nor
-- read back. The row would then be visible to A while its kind belongs to B --
-- and a booking born from it snapshots B's price (BR-014). Silent, because
-- nothing errors.
--
-- Pair the columns instead. Exactly the shape and the reasoning of
-- refresh_tokens_user_matches_owner in 000002.
ALTER TABLE resource_units
    ADD CONSTRAINT resource_units_resource_matches_owner
    FOREIGN KEY (resource_id, owner_id) REFERENCES resources (id, owner_id);

-- BR-011: unique per owner, and a deleted unit releases its code again --
-- a plate that moved to another car has to be usable on that car.
CREATE UNIQUE INDEX resource_units_code_per_owner
    ON resource_units (owner_id, code) WHERE deleted_at IS NULL;

-- PRD section 9: availability over 500 units x 12 months, under a second. The
-- lookup is always "this rental's units of this kind that are usable".
CREATE INDEX resource_units_owner_resource_status
    ON resource_units (owner_id, resource_id, status) WHERE deleted_at IS NULL;

SELECT enable_owner_rls('resource_units');
