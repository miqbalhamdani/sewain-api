-- Local use only. Migrations are forward-only in production -- see CLAUDE.md.

DROP FUNCTION IF EXISTS enable_owner_rls(regclass);

ALTER DEFAULT PRIVILEGES IN SCHEMA public
    REVOKE SELECT, INSERT, UPDATE, DELETE ON TABLES FROM app_user;

REVOKE USAGE ON SCHEMA public FROM app_user;

-- app_user is deliberately NOT dropped. A role is cluster-wide: dropping it here
-- would break every other database in the cluster that granted anything to it,
-- and it cannot be dropped at all while such a grant exists. Reversing this
-- migration returns the schema to its previous state, not the cluster.

-- Extensions are left in place for the same reason -- they are cheap, other
-- databases may share them, and dropping btree_gist would cascade into the
-- exclusion constraint that BR-022 depends on.
