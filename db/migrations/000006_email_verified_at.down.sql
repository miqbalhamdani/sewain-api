DROP INDEX IF EXISTS users_unverified;
ALTER TABLE users DROP COLUMN IF EXISTS email_verified_at;
