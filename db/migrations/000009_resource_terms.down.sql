-- Local use only. Migrations are forward-only in production -- see CLAUDE.md.
ALTER TABLE resources
    DROP CONSTRAINT IF EXISTS resources_description_length,
    DROP CONSTRAINT IF EXISTS resources_terms_excludes_length,
    DROP CONSTRAINT IF EXISTS resources_terms_requirements_length,
    DROP CONSTRAINT IF EXISTS resources_terms_cancellation_length;
ALTER TABLE resources
    DROP COLUMN IF EXISTS description,
    DROP COLUMN IF EXISTS terms_excludes,
    DROP COLUMN IF EXISTS terms_requirements,
    DROP COLUMN IF EXISTS terms_cancellation;
