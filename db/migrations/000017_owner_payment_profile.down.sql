-- Local use only. Migrations are forward-only in production -- see CLAUDE.md.
ALTER TABLE owners DROP CONSTRAINT IF EXISTS owners_bank_account_number_format;
ALTER TABLE owners
    DROP COLUMN IF EXISTS bank_name,
    DROP COLUMN IF EXISTS bank_account_number,
    DROP COLUMN IF EXISTS bank_account_holder,
    DROP COLUMN IF EXISTS allowed_origins;
