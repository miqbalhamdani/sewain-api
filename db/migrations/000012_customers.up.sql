-- Penyewa dan jejak audit.  (S1-020, S1-021)
--
-- Transcribed from ../docs/03-erd.md sections 1 and 3. Where this file and those
-- sections disagree, this file is the bug.

CREATE TABLE customers (
    id       uuid PRIMARY KEY,
    owner_id uuid NOT NULL REFERENCES owners(id),

    name  text NOT NULL,
    phone text NOT NULL,

    -- BR-085. The number is encrypted by the application (AES-256-GCM, key in
    -- IDENTITY_ENC_KEY, never in this database) because no screen ever needs it
    -- rendered whole. last4 is kept beside it in the clear so a list can show
    -- "KTP ...7890" without decrypting a page of rows -- four digits identify
    -- nobody, and matching the card in someone's hand is all the counter needs.
    id_type         text,
    id_number_enc   bytea,
    id_number_last4 text,

    -- An object key, not a URL. NOT application-encrypted: the signed URL has to
    -- render straight in a browser, so R2's at-rest encryption is the layer
    -- (BR-085). Written by S1-033's upload path; nothing writes it yet.
    id_photo_key text,

    -- BR-028. The reason is what an operator reads when the system refuses a
    -- booking, so blocked-without-reason is refused by the CHECK below.
    is_blacklisted   boolean NOT NULL DEFAULT false,
    blacklist_reason text,

    -- BR-086, deferred: no job reads it in phase 1.
    id_purge_after timestamptz,

    created_by uuid REFERENCES users(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    deleted_at timestamptz
);

-- Named in ALTERs, not inline: internal/customer matches on constraint names.
ALTER TABLE customers
    ADD CONSTRAINT customers_id_type_valid
        CHECK (id_type IS NULL OR id_type IN ('ktp', 'sim', 'passport')),
    ADD CONSTRAINT customers_blacklist_has_reason
        CHECK (is_blacklisted = (blacklist_reason IS NOT NULL));

-- Target for bookings_customer_matches_owner in 000013. FK checks bypass RLS,
-- so the pair is what stops a booking naming another rental's customer.
ALTER TABLE customers ADD CONSTRAINT customers_id_owner_uq UNIQUE (id, owner_id);

-- Newest first is the only order the list is read in; the id is UUID v7, so it
-- is the creation order and the cursor at once.
CREATE INDEX customers_owner_id ON customers (owner_id, id DESC) WHERE deleted_at IS NULL;

SELECT enable_owner_rls('customers');

-- ---------------------------------------------------------------------------
-- audit_logs -- append-only, and the database is what says so
-- ---------------------------------------------------------------------------

CREATE TABLE audit_logs (
    id            uuid PRIMARY KEY,
    owner_id      uuid NOT NULL REFERENCES owners(id),
    actor_user_id uuid NOT NULL,
    action        text NOT NULL,   -- e.g. customer.identity.viewed (BR-085)
    entity        text NOT NULL,
    entity_id     uuid NOT NULL,
    metadata      jsonb NOT NULL DEFAULT '{}',
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- Same shape and reason as refresh_tokens_user_matches_owner.
ALTER TABLE audit_logs ADD CONSTRAINT audit_logs_actor_matches_owner
    FOREIGN KEY (actor_user_id, owner_id) REFERENCES users (id, owner_id);

CREATE INDEX audit_logs_owner_entity ON audit_logs (owner_id, entity, entity_id);

SELECT enable_owner_rls('audit_logs');

-- "No UPDATE or DELETE query in db/queries/" is a discipline. This makes it a
-- refusal: the default privileges in 000001 granted both, and a path that
-- forgets the discipline now gets an error instead of a rewritable trail.
REVOKE UPDATE, DELETE ON audit_logs FROM app_user;
