-- Identity: owners, users, refresh_tokens.  (S1-007)
--
-- Transcribed from ../docs/03-erd.md sections 1 and 3. Where this file and those
-- sections disagree, this file is the bug.
--
-- One user belongs to exactly one rental (BR-004). `users.owner_id` says which,
-- `users.role` says what they may do there. There is no membership table and no
-- owner switcher -- a design that was tried and reversed, so if you find code
-- that assumes otherwise it is code written against the old contract.

-- ---------------------------------------------------------------------------
-- owners -- the tenant itself, so no owner_id and no RLS
-- ---------------------------------------------------------------------------

CREATE TABLE owners (
    id     uuid PRIMARY KEY,
    slug   text NOT NULL,
    name   text NOT NULL,
    status text NOT NULL DEFAULT 'active'
           CHECK (status IN ('active','suspended')),

    -- Owner knobs. Every one of these exists because a BR forbids hard-coding
    -- it, and PATCH /settings (S1-009) is the only place they are set.
    require_payment_before_pickup bool NOT NULL DEFAULT false,  -- BR-038
    draft_expiry_hours            int  NOT NULL DEFAULT 24,     -- BR-027
    notify_pickup_reminder        bool NOT NULL DEFAULT true,   -- BR-070
    notify_return_reminder        bool NOT NULL DEFAULT true,   -- BR-070
    notify_overdue_reminder       bool NOT NULL DEFAULT true,   -- BR-070

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- slug IS the subdomain label, so it answers to DNS label rules (BR-025).
-- Plain column, NOT lower(slug): an expression index cannot serve WHERE slug = $1,
-- and that is the hottest lookup in the system -- every public page request
-- resolves Host -> slug -> owner.
CREATE UNIQUE INDEX owners_slug_unique ON owners (slug);

-- 3-63 characters, letter/digit/hyphen, no leading or trailing hyphen.
ALTER TABLE owners ADD CONSTRAINT owners_slug_format
    CHECK (slug ~ '^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$');

-- Positions 3-4 are punycode space (xn--). Leaving it open invites registering
-- a slug that renders as somebody else's domain.
ALTER TABLE owners ADD CONSTRAINT owners_slug_not_punycode
    CHECK (substring(slug, 3, 2) <> '--');

-- What the /r/ path prefix used to do: name collisions moved from route space
-- to subdomain space, so the reserved list has to move with them. Enforced by
-- the database, not by application validation -- an application check is one
-- forgotten code path away from handing out `api`.
ALTER TABLE owners ADD CONSTRAINT owners_slug_not_reserved
    CHECK (slug NOT IN (
        'app','api','www','admin','auth','login','dashboard','portal','settings',
        'static','assets','cdn','media','img','files','mail','smtp','imap','mx',
        'ns1','ns2','blog','status','help','docs','support','billing','pay',
        'checkout','webhook','webhooks','test','staging','dev','demo','sewain'));

-- ---------------------------------------------------------------------------
-- users
-- ---------------------------------------------------------------------------

CREATE TABLE users (
    id            uuid PRIMARY KEY,
    owner_id      uuid NOT NULL REFERENCES owners(id),
    email         citext NOT NULL,
    password_hash text,                 -- null while an invitation is outstanding
    name          text NOT NULL,
    phone         text,

    -- One role per person, because one person has one rental (BR-003, BR-004).
    role   text NOT NULL DEFAULT 'operator'
           CHECK (role IN ('owner','operator')),
    status text NOT NULL DEFAULT 'invited'
           CHECK (status IN ('invited','active','disabled')),

    last_login_at timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

-- Email is unique across the whole system, not per rental (BR-004).
--
-- POST /auth/login carries only an email and a password. With the same address at
-- two rentals there would be nothing in the request to choose between them, so the
-- user row has to be the thing that says which rental it belongs to. This is what
-- buys login its missing parameter, and the price is that one address means one
-- rental -- somebody running two needs two accounts.
ALTER TABLE users ADD CONSTRAINT users_email_key UNIQUE (email);

SELECT enable_owner_rls('users');

-- ---------------------------------------------------------------------------
-- refresh_tokens
-- ---------------------------------------------------------------------------

CREATE TABLE refresh_tokens (
    id         uuid PRIMARY KEY,
    owner_id   uuid NOT NULL REFERENCES owners(id),
    user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash text NOT NULL,           -- SHA-256; the plaintext lives only in the cookie

    -- Rotation chain: a reused (already-rotated) token means theft. Revoke the
    -- whole chain rather than just rejecting the one request.
    rotated_from uuid REFERENCES refresh_tokens(id),

    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT refresh_tokens_token_hash_key UNIQUE (token_hash)
);

CREATE INDEX refresh_tokens_active ON refresh_tokens (user_id) WHERE revoked_at IS NULL;

SELECT enable_owner_rls('refresh_tokens');

-- refresh_tokens carries owner_id AND user_id, and nothing so far forces the two
-- to agree. A row where they disagree mints an access token for the wrong rental
-- -- and nothing errors, which is the whole problem. The composite FK below makes
-- that row impossible to write.
--
-- users_id_owner_uq exists only as its target: PostgreSQL requires an exactly
-- matching unique constraint, and `id` already being the primary key does not
-- satisfy a two-column reference.
ALTER TABLE users ADD CONSTRAINT users_id_owner_uq UNIQUE (id, owner_id);
ALTER TABLE refresh_tokens ADD CONSTRAINT refresh_tokens_user_matches_owner
    FOREIGN KEY (user_id, owner_id) REFERENCES users (id, owner_id);
