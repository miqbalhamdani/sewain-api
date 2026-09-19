-- Bootstrap: extensions, the non-owning application role, and the RLS helper.
--
-- Nothing here creates a table. This migration exists so that every migration
-- after it can assume the extensions are present, that a new table is granted to
-- app_user automatically, and that turning on owner isolation is one line.

-- ---------------------------------------------------------------------------
-- Extensions (../docs/03-erd.md 3)
-- ---------------------------------------------------------------------------

-- btree_gist is not optional and not a nicety: bookings_no_overlap is a GiST
-- exclusion constraint over (unit_id, tstzrange), and GiST cannot index the uuid
-- equality half without it. BR-022 -- the one claim this whole product makes --
-- does not exist without this line.
CREATE EXTENSION IF NOT EXISTS btree_gist;

CREATE EXTENSION IF NOT EXISTS citext;     -- users.email, unique GLOBALLY (BR-004)

-- Deliberately absent: ltree, unaccent, pg_trgm. Those belong to a product
-- catalogue with nested categories and title search. Sewain has neither.

-- ---------------------------------------------------------------------------
-- app_user (BR-001)
-- ---------------------------------------------------------------------------
--
-- The application connects as this role. It owns nothing, which is the whole
-- point: FORCE ROW LEVEL SECURITY makes a policy apply to the table owner too,
-- but a role that owns nothing can never reach for owner-only escapes -- it
-- cannot ALTER a table to drop a policy, and it cannot become the owner.
--
-- Migrations run as the schema owner, not as this role.

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_user') THEN
        -- No password. Local development authenticates by trust; every deployed
        -- environment sets one out of band, so a credential never lands in a
        -- migration file or in git.
        CREATE ROLE app_user LOGIN;
    END IF;
END
$$;

GRANT USAGE ON SCHEMA public TO app_user;

-- Default privileges rather than GRANT ... ON ALL TABLES. The ON ALL form only
-- covers tables that exist at the moment it runs, so every future schema item
-- would have to remember to re-grant, and the one that forgets produces a
-- permission error in production rather than in review.
--
-- These apply to objects created by the role running this migration. That role
-- must stay the same across environments -- it is the schema owner.
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO app_user;

-- Deliberately no GRANT on sequences. Keys are UUID v7 generated in Go; this
-- schema has no sequences and is not going to grow any.

-- ---------------------------------------------------------------------------
-- enable_owner_rls (BR-001, ../docs/03-erd.md 3)
-- ---------------------------------------------------------------------------
--
-- The pattern is four statements and every owner table needs all four. Copied
-- by hand across a dozen migrations, one of them eventually gets three of the
-- four right -- and the failure mode of a missing FORCE is silent: everything
-- works, and the schema owner quietly sees every rental's rows.
--
-- So a table migration says:
--
--     SELECT enable_owner_rls('bookings');
--
-- INVOKER security, not DEFINER: the caller must already own the table. app_user
-- calling this gets a permission error, which is correct.

CREATE OR REPLACE FUNCTION enable_owner_rls(target regclass)
RETURNS void
LANGUAGE plpgsql
AS $$
BEGIN
    -- Without owner_id the policy below would raise on every row touched.
    -- Better to fail here, while someone is looking at the migration.
    IF NOT EXISTS (
        SELECT 1 FROM pg_attribute
        WHERE attrelid = target
          AND attname  = 'owner_id'
          AND attnum   > 0
          AND NOT attisdropped
    ) THEN
        RAISE EXCEPTION
            'enable_owner_rls: % has no owner_id column', target;
    END IF;

    EXECUTE format('ALTER TABLE %s ENABLE ROW LEVEL SECURITY', target);
    EXECUTE format('ALTER TABLE %s FORCE  ROW LEVEL SECURITY', target);

    -- NULLIF is required, not defensive. current_setting(..., true) returns NULL
    -- when app.owner_id was never set -- good, the comparison is NULL, every row
    -- filters out, RLS fails closed. But it returns '' when the setting was set
    -- to an empty string, and ''::uuid raises. internal/db turns the closed case
    -- into ErrNoOwnerContext (S1-004) so it presents as an error rather than as
    -- a baffling empty result.
    --
    -- WITH CHECK is not a duplicate of USING. USING is NOT applied to INSERT:
    -- with USING alone, reading another rental's row is impossible but WRITING
    -- one succeeds silently, and the row then vanishes from its own author's
    -- view. Proven both ways by ../docs/03-verify-with-check.sql.
    EXECUTE format($policy$
        CREATE POLICY owner_isolation ON %s
            USING      (owner_id = NULLIF(current_setting('app.owner_id', true), '')::uuid)
            WITH CHECK (owner_id = NULLIF(current_setting('app.owner_id', true), '')::uuid)
    $policy$, target);
END;
$$;

COMMENT ON FUNCTION enable_owner_rls(regclass) IS
    'Enable + FORCE row level security and the owner_isolation policy on a table with an owner_id column. See BR-001 and ../docs/03-erd.md 3.';
