-- Kunci API eksternal.  (S1-079, BR-031, BR-032)
--
-- Pemilik yang meng-host situsnya sendiri menarik katalog lewat api.sewain.id
-- dengan kunci ini. Ia hanya membuka empat endpoint /public/* -- bukan pintu ke
-- backoffice -- dan rahasianya tidak pernah disimpan: hash argon2id plus 8
-- karakter prefix untuk dikenali. Dicabut, tidak dihapus: log akses lama harus
-- tetap bisa dijelaskan.

CREATE TABLE api_keys (
    id                 uuid PRIMARY KEY,
    owner_id           uuid NOT NULL REFERENCES owners(id),
    name               text NOT NULL,
    key_prefix         text NOT NULL,
    key_hash           text NOT NULL,
    rate_limit_per_min int  NOT NULL DEFAULT 60,
    last_used_at       timestamptz,
    revoked_at         timestamptz,
    created_by         uuid,
    created_at         timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT api_keys_name_length CHECK (char_length(name) BETWEEN 1 AND 80),
    CONSTRAINT api_keys_prefix_format CHECK (key_prefix ~ '^[a-z0-9]{8}$'),
    CONSTRAINT api_keys_rate_limit_range CHECK (rate_limit_per_min BETWEEN 1 AND 600),
    -- The prefix is how a presented key finds its row, across every owner.
    CONSTRAINT api_keys_prefix_unique UNIQUE (key_prefix),
    CONSTRAINT api_keys_created_by_matches_owner
        FOREIGN KEY (created_by, owner_id) REFERENCES users (id, owner_id)
);

CREATE INDEX api_keys_owner_created ON api_keys (owner_id, created_at DESC);

SELECT enable_owner_rls('api_keys');

-- The third lookup.  000003 said "There will not be a third. Adding one is a
-- conversation, not a line." This is the conversation's outcome (M5 fase C,
-- 05-backlog.md): a key reaches api.sewain.id with no owner context -- exactly
-- the login problem -- and BR-030 already names the key as the second tokenless
-- path, so this is a hinge on an existing door rather than a new one. Same
-- discipline: owned by the BYPASSRLS role that owns nothing else, columns fixed
-- at definition, no name and no creator in the result.
GRANT SELECT (id, owner_id, key_prefix, key_hash, rate_limit_per_min, revoked_at)
    ON api_keys TO auth_lookup;

CREATE FUNCTION auth_lookup_api_key(p_prefix text)
RETURNS TABLE (
    id                 uuid,
    owner_id           uuid,
    key_hash           text,
    rate_limit_per_min int,
    revoked_at         timestamptz
)
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
    SELECT k.id, k.owner_id, k.key_hash, k.rate_limit_per_min, k.revoked_at
      FROM api_keys k
     WHERE k.key_prefix = p_prefix;
$$;

ALTER FUNCTION auth_lookup_api_key(text) OWNER TO auth_lookup;
REVOKE ALL ON FUNCTION auth_lookup_api_key(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION auth_lookup_api_key(text) TO app_user;

COMMENT ON FUNCTION auth_lookup_api_key(text) IS
    'The external API path, before any owner context exists. Five fixed columns. See BR-031.';
