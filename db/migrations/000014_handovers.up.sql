-- Bukti kondisi: serah-terima ambil dan kembali.  (S1-034)
--
-- Transcribed from ../docs/03-erd.md sections 1 and 3. Where this file and those
-- sections disagree, this file is the bug.
--
-- Append-only, and the database is what says so: no deleted_at, no update
-- query, and UPDATE/DELETE revoked from app_user at the bottom (BR-037).

-- Target for the handover FK below and for invoices in 000015.
ALTER TABLE bookings ADD CONSTRAINT bookings_id_owner_uq UNIQUE (id, owner_id);

CREATE TABLE handovers (
    id           uuid PRIMARY KEY,
    owner_id     uuid NOT NULL REFERENCES owners(id),
    booking_id   uuid NOT NULL,
    direction    text NOT NULL,
    performed_by uuid NOT NULL,
    performed_at timestamptz NOT NULL DEFAULT now(),

    -- Odometer for vehicles (BR-036). "Metered" = the resource has a
    -- vehicle_specs row, decided at S1-035; there is no is_metered column.
    meter_value     bigint,
    checklist       jsonb NOT NULL DEFAULT '{}',
    condition_notes text,

    -- BR-051: what was waived of the late fee, and why. On the handover rather
    -- than an invoice line, because a full waiver has no line to carry it.
    late_fee_waived bigint,
    waiver_reason   text,

    created_at timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE handovers
    ADD CONSTRAINT handovers_direction_valid CHECK (direction IN ('pickup', 'return')),
    ADD CONSTRAINT handovers_waiver_complete
        CHECK (num_nonnulls(late_fee_waived, waiver_reason) IN (0, 2)
               AND (late_fee_waived IS NULL OR (direction = 'return' AND late_fee_waived >= 0))),
    ADD CONSTRAINT handovers_id_owner_uq UNIQUE (id, owner_id),
    ADD CONSTRAINT handovers_booking_matches_owner
        FOREIGN KEY (booking_id, owner_id) REFERENCES bookings (id, owner_id),
    ADD CONSTRAINT handovers_performer_matches_owner
        FOREIGN KEY (performed_by, owner_id) REFERENCES users (id, owner_id);

-- BR-035: one per direction per booking.
CREATE UNIQUE INDEX handovers_one_per_direction ON handovers (booking_id, direction);

CREATE TABLE handover_photos (
    id          uuid PRIMARY KEY,
    owner_id    uuid NOT NULL REFERENCES owners(id),
    handover_id uuid NOT NULL,
    -- A key under handovers/<owner>/<booking>/, never a URL (BR-093).
    object_key  text NOT NULL,
    captured_at timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE handover_photos
    ADD CONSTRAINT handover_photos_id_owner_uq UNIQUE (id, owner_id),
    ADD CONSTRAINT handover_photos_handover_matches_owner
        FOREIGN KEY (handover_id, owner_id) REFERENCES handovers (id, owner_id);

CREATE INDEX handover_photos_handover ON handover_photos (owner_id, handover_id);

SELECT enable_owner_rls('handovers');
SELECT enable_owner_rls('handover_photos');

REVOKE UPDATE, DELETE ON handovers, handover_photos FROM app_user;
