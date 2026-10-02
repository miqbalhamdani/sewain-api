-- Invoice dan barisnya.  (S1-041, pulled forward from M4 into M3; S1-043)
--
-- Transcribed from ../docs/03-erd.md sections 1 and 3. Where this file and those
-- sections disagree, this file is the bug.
--
-- There is no total column, and there will not be one: a total is
-- SUM(invoice_lines.amount), read when needed (BR-055, 03-erd.md section 4).

CREATE TABLE invoices (
    id       uuid PRIMARY KEY,
    owner_id uuid NOT NULL REFERENCES owners(id),

    -- BR-082: one table for rent and subscription. Subscriptions are deferred;
    -- the column stays so switching them on needs no migration.
    kind            text NOT NULL DEFAULT 'booking',
    booking_id      uuid,
    subscription_id uuid,
    customer_id     uuid,

    -- "<booking code>/<n>", unique per owner.
    number  text NOT NULL,
    status  text NOT NULL DEFAULT 'unpaid',
    -- BR-057: always set by the server when issued, never by a client.
    due_at  timestamptz NOT NULL,
    paid_at timestamptz,

    created_by uuid REFERENCES users(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    deleted_at timestamptz
);

ALTER TABLE invoices
    ADD CONSTRAINT invoices_status_valid
        CHECK (status IN ('unpaid', 'gateway_pending', 'paid', 'overdue', 'cancelled')),
    ADD CONSTRAINT invoices_kind_valid CHECK (kind IN ('booking', 'subscription')),
    ADD CONSTRAINT invoices_one_subject CHECK (num_nonnulls(booking_id, subscription_id) = 1),
    ADD CONSTRAINT invoices_id_owner_uq UNIQUE (id, owner_id),
    ADD CONSTRAINT invoices_booking_matches_owner
        FOREIGN KEY (booking_id, owner_id) REFERENCES bookings (id, owner_id),
    ADD CONSTRAINT invoices_customer_matches_owner
        FOREIGN KEY (customer_id, owner_id) REFERENCES customers (id, owner_id);

CREATE UNIQUE INDEX invoices_number_per_owner ON invoices (owner_id, number);
CREATE INDEX invoices_owner_booking ON invoices (owner_id, booking_id);

CREATE TABLE invoice_lines (
    id                uuid PRIMARY KEY,
    owner_id          uuid NOT NULL REFERENCES owners(id),
    invoice_id        uuid NOT NULL,
    kind              text NOT NULL,
    description       text NOT NULL,
    amount            bigint NOT NULL,
    handover_photo_id uuid,
    waiver_reason     text,
    created_at        timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE invoice_lines
    ADD CONSTRAINT invoice_lines_kind_valid
        CHECK (kind IN ('rent', 'deposit', 'late_fee', 'damage', 'discount')),
    -- BR-055: discount negative, everything else positive.
    ADD CONSTRAINT invoice_lines_sign
        CHECK ((kind = 'discount' AND amount < 0) OR (kind <> 'discount' AND amount > 0)),
    -- BR-047, S1-043: a damage line names a return photo, refused by the
    -- database rather than only by the app.
    ADD CONSTRAINT invoice_lines_damage_needs_photo
        CHECK (kind <> 'damage' OR handover_photo_id IS NOT NULL),
    ADD CONSTRAINT invoice_lines_invoice_matches_owner
        FOREIGN KEY (invoice_id, owner_id) REFERENCES invoices (id, owner_id),
    ADD CONSTRAINT invoice_lines_photo_matches_owner
        FOREIGN KEY (handover_photo_id, owner_id) REFERENCES handover_photos (id, owner_id);

CREATE INDEX invoice_lines_owner_invoice ON invoice_lines (owner_id, invoice_id);

SELECT enable_owner_rls('invoices');
SELECT enable_owner_rls('invoice_lines');
