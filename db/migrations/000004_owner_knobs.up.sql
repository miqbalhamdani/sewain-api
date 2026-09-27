-- Owner knobs that 03-erd.md section 1 requires and 000002 did not carry.  (S1-009)
--
-- A separate migration rather than an edit to 000002: that one has been applied,
-- and forward-only is the rule the moment a migration has run anywhere.
--
-- Deliberately NOT here:
--   business_type    -- S1-082 owns it; it is chosen once at registration (BR-017)
--   allowed_origins  -- S1-079 owns it; it is set from the API-keys screen (BR-031)
-- Adding either now would enforce half a rule.

ALTER TABLE owners
    ADD COLUMN booking_code_prefix     text NOT NULL DEFAULT 'SWN',
    ADD COLUMN payment_due_hours       int  NOT NULL DEFAULT 24,
    ADD COLUMN no_show_tolerance_hours int  NOT NULL DEFAULT 3;

-- 2-6 uppercase letters or digits. The prefix is read when a booking code is
-- minted, never afterwards, so changing it leaves issued codes alone and does
-- not reset the counter (BR-024).
ALTER TABLE owners ADD CONSTRAINT owners_booking_code_prefix_format
    CHECK (booking_code_prefix ~ '^[A-Z0-9]{2,6}$');

-- The two halves of BR-057, and they differ on purpose.
--
-- payment_due_hours = 0 would issue an invoice that is already past due at the
-- instant it is created, and the expiry job would cancel the booking on its
-- next tick. There is no owner who wants that, so the database refuses it.
ALTER TABLE owners ADD CONSTRAINT owners_payment_due_hours_positive
    CHECK (payment_due_hours > 0);

-- no_show_tolerance_hours = 0 is a real setting: an owner who frees the unit
-- the moment the booking starts sets exactly this. Zero is accepted, negative
-- is not.
ALTER TABLE owners ADD CONSTRAINT owners_no_show_tolerance_non_negative
    CHECK (no_show_tolerance_hours >= 0);

-- No enable_owner_rls here. owners IS the tenant -- it has no owner_id, so
-- there is nothing to isolate it against, and `make lint-rls` skips it for
-- exactly that reason (03-erd.md section 2).
