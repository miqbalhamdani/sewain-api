-- Local use only. Migrations are forward-only in production -- see CLAUDE.md.
ALTER TABLE owners DROP CONSTRAINT IF EXISTS owners_whatsapp_format;
ALTER TABLE owners
    DROP COLUMN IF EXISTS whatsapp,
    DROP COLUMN IF EXISTS address,
    DROP COLUMN IF EXISTS operating_hours;
