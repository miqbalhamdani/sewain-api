-- Uang: penyelesaian deposit, pembayaran, bukti transfer.  (S1-042, S1-044, S1-046)
--
-- Transcribed from ../docs/03-erd.md sections 1 and 3. Where this file and those
-- sections disagree, this file is the bug.

-- BR-048 + BR-049: "settled" is a column, not a guess from 0/0 -- and once set,
-- deducted + refunded is exactly the deposit. A waived deposit is never
-- settled, deducted or refunded: the money never came in (BR-051).
ALTER TABLE bookings ADD COLUMN deposit_settled_at timestamptz;
ALTER TABLE bookings
    ADD CONSTRAINT bookings_deposit_settlement
        CHECK (deposit_settled_at IS NULL
               OR (deposit_amount IS NOT NULL AND deposit_waived_at IS NULL
                   AND deposit_deducted + deposit_refunded = deposit_amount)),
    ADD CONSTRAINT bookings_deposit_waived_untouched
        CHECK (deposit_waived_at IS NULL OR (deposit_deducted = 0 AND deposit_refunded = 0));

CREATE TABLE payments (
    id          uuid PRIMARY KEY,
    owner_id    uuid NOT NULL REFERENCES owners(id),
    invoice_id  uuid NOT NULL,
    -- 'gateway' stays in the vocabulary and is unreachable in phase 1 (BR-061).
    method      text NOT NULL,
    amount      bigint NOT NULL,
    status      text NOT NULL,
    paid_at     timestamptz NOT NULL,
    approved_by uuid NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE payments
    ADD CONSTRAINT payments_method_valid CHECK (method IN ('gateway', 'manual_transfer', 'cash')),
    ADD CONSTRAINT payments_status_valid CHECK (status IN ('success', 'failed')),
    ADD CONSTRAINT payments_amount_positive CHECK (amount > 0),
    ADD CONSTRAINT payments_id_owner_uq UNIQUE (id, owner_id),
    ADD CONSTRAINT payments_invoice_matches_owner
        FOREIGN KEY (invoice_id, owner_id) REFERENCES invoices (id, owner_id),
    ADD CONSTRAINT payments_approver_matches_owner
        FOREIGN KEY (approved_by, owner_id) REFERENCES users (id, owner_id);

-- BR-060: one successful payment per invoice. Two operators recording the
-- same cash in the same second get one row and one 409, decided here.
CREATE UNIQUE INDEX payments_one_success_per_invoice ON payments (invoice_id) WHERE status = 'success';

CREATE TABLE payment_proofs (
    id           uuid PRIMARY KEY,
    owner_id     uuid NOT NULL REFERENCES owners(id),
    invoice_id   uuid NOT NULL,
    -- A key under proofs/<owner>/<invoice>/, never a URL (BR-093).
    object_key   text NOT NULL,
    content_type text NOT NULL,

    -- BR-062: the automatic reading is a RECOMMENDATION and may be absent --
    -- phase 1 has no model, so these stay NULL ("not read, check by hand").
    match_status text,
    ai_amount    bigint,
    ai_paid_at   timestamptz,

    -- The human decision, in its own columns.
    review_status text NOT NULL DEFAULT 'pending',
    reviewed_by   uuid,
    reviewed_at   timestamptz,
    reject_reason text,
    payment_id    uuid,

    created_by uuid REFERENCES users(id),
    created_at timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE payment_proofs
    ADD CONSTRAINT payment_proofs_review_valid
        CHECK (review_status IN ('pending', 'approved', 'rejected')),
    ADD CONSTRAINT payment_proofs_match_valid
        CHECK (match_status IS NULL OR match_status IN ('match', 'mismatch', 'unreadable')),
    ADD CONSTRAINT payment_proofs_reviewed_complete
        CHECK ((review_status = 'pending') = (reviewed_at IS NULL)
               AND num_nonnulls(reviewed_by, reviewed_at) IN (0, 2)),
    ADD CONSTRAINT payment_proofs_reject_reason
        CHECK ((review_status = 'rejected') = (reject_reason IS NOT NULL)),
    ADD CONSTRAINT payment_proofs_approved_has_payment
        CHECK ((review_status = 'approved') = (payment_id IS NOT NULL)),
    ADD CONSTRAINT payment_proofs_invoice_matches_owner
        FOREIGN KEY (invoice_id, owner_id) REFERENCES invoices (id, owner_id),
    ADD CONSTRAINT payment_proofs_payment_matches_owner
        FOREIGN KEY (payment_id, owner_id) REFERENCES payments (id, owner_id);

CREATE INDEX payment_proofs_owner_invoice ON payment_proofs (owner_id, invoice_id);
CREATE INDEX payments_owner_invoice ON payments (owner_id, invoice_id);
-- The owner's full invoice list (S1-047): newest first.
CREATE INDEX invoices_owner_created ON invoices (owner_id, created_at DESC, id DESC) WHERE deleted_at IS NULL;

SELECT enable_owner_rls('payments');
SELECT enable_owner_rls('payment_proofs');
