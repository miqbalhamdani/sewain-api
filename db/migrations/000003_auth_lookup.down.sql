-- Local use only. Migrations are forward-only in production -- see CLAUDE.md.

DROP FUNCTION IF EXISTS auth_lookup_refresh_token(text);
DROP FUNCTION IF EXISTS auth_lookup_user(citext);

REVOKE ALL (id, owner_id, user_id, token_hash, expires_at, revoked_at)
    ON refresh_tokens FROM auth_lookup;
REVOKE ALL (id, owner_id, password_hash, status, role, email) ON users FROM auth_lookup;

-- auth_lookup is deliberately not dropped: a role is cluster-wide, and dropping
-- one here can break unrelated databases that granted to it. Same reasoning as
-- app_user in 000001.
