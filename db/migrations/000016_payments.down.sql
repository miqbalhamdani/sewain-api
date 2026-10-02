-- Local use only. Migrations are forward-only in production -- see CLAUDE.md.
DROP TABLE IF EXISTS payment_proofs;
DROP TABLE IF EXISTS payments;
DROP INDEX IF EXISTS invoices_owner_created;
ALTER TABLE bookings DROP CONSTRAINT IF EXISTS bookings_deposit_waived_untouched;
ALTER TABLE bookings DROP CONSTRAINT IF EXISTS bookings_deposit_settlement;
ALTER TABLE bookings DROP COLUMN IF EXISTS deposit_settled_at;
