-- The two reads that run before a rental is known.  (S1-008)
--
-- Login has only an email; refresh has only a cookie -- that is the whole reason
-- refresh is being called. Both `users` and `refresh_tokens` carry owner_id and so
-- both carry RLS (000002), which means a plain query at that point compares
-- owner_id against a NULL setting and matches zero rows. Something has to reach
-- past that, exactly twice.
--
-- SECURITY DEFINER alone is NOT enough. FORCE ROW LEVEL SECURITY binds the table's
-- owner too, so a definer function owned by the schema owner is filtered like
-- anyone else -- unless that owner happens to be a superuser, which is true on a
-- developer's machine and must not be relied on in production. Both functions
-- therefore belong to a role that carries BYPASSRLS and owns nothing else.
--
-- What keeps them small is the return type, fixed at definition time. Neither
-- returns a name or an email, so neither can be turned into a cross-rental
-- directory. Widening either is a schema change AND a contract change.
--
-- There will not be a third. Adding one is a conversation, not a line.

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'auth_lookup') THEN
        -- NOLOGIN: nothing connects as this role. It exists to own two functions.
        CREATE ROLE auth_lookup NOLOGIN BYPASSRLS;
    END IF;
END
$$;

GRANT SELECT (id, owner_id, password_hash, status, role, email) ON users TO auth_lookup;
GRANT SELECT (id, owner_id, user_id, token_hash, expires_at, revoked_at)
    ON refresh_tokens TO auth_lookup;

-- Owning a function requires membership in the owning role. The role's creator
-- holds ADMIN OPTION on it, so this is a no-op on a fresh database and a
-- deliberate re-grant on one where the role already existed.
DO $$
BEGIN
    EXECUTE format('GRANT auth_lookup TO %I', current_user);
END
$$;

CREATE FUNCTION auth_lookup_user(p_email citext)
RETURNS TABLE (
    id            uuid,
    owner_id      uuid,
    password_hash text,
    status        text,
    role          text
)
LANGUAGE sql
STABLE
SECURITY DEFINER
-- Pinned so a caller cannot shadow `users` with a table of their own earlier in
-- the search path. Mandatory on any SECURITY DEFINER function.
SET search_path = pg_catalog, public
AS $$
    SELECT u.id, u.owner_id, u.password_hash, u.status, u.role
      FROM users u
     WHERE u.email = p_email;
$$;

-- The rejected alternative for refresh was putting the owner id in the cookie next
-- to the token, which needs no function at all. It works, because a wrong owner
-- makes the hash lookup miss -- but it means the owner arrives in the request, and
-- BR-001 says the owner is derived from the token and never accepted from the
-- request. One more narrow function in the same place is more consistent than a
-- second, different mechanism.
--
-- Returns no token material: the caller already holds the token.
CREATE FUNCTION auth_lookup_refresh_token(p_token_hash text)
RETURNS TABLE (
    id         uuid,
    owner_id   uuid,
    user_id    uuid,
    expires_at timestamptz,
    revoked_at timestamptz
)
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
    SELECT t.id, t.owner_id, t.user_id, t.expires_at, t.revoked_at
      FROM refresh_tokens t
     WHERE t.token_hash = p_token_hash;
$$;

ALTER FUNCTION auth_lookup_user(citext)        OWNER TO auth_lookup;
ALTER FUNCTION auth_lookup_refresh_token(text) OWNER TO auth_lookup;

-- EXECUTE is granted to PUBLIC by default on a new function, which would make
-- these readable by anything that can reach the database.
REVOKE ALL ON FUNCTION auth_lookup_user(citext)        FROM PUBLIC;
REVOKE ALL ON FUNCTION auth_lookup_refresh_token(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION auth_lookup_user(citext)        TO app_user;
GRANT EXECUTE ON FUNCTION auth_lookup_refresh_token(text) TO app_user;

COMMENT ON FUNCTION auth_lookup_user(citext) IS
    'The login path, before any owner context exists. Five fixed columns. See BR-004.';
COMMENT ON FUNCTION auth_lookup_refresh_token(text) IS
    'Resolves a refresh token to its owner so rotation can proceed inside InOwnerTx. Returns no token material.';
