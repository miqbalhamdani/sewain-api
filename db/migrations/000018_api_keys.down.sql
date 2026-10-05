-- Local use only. Migrations are forward-only in production -- see CLAUDE.md.
DROP FUNCTION IF EXISTS auth_lookup_api_key(text);
DROP TABLE IF EXISTS api_keys;
