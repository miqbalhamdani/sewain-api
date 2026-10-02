-- Local use only. Migrations are forward-only in production -- see CLAUDE.md.
DROP TABLE IF EXISTS handover_photos;
DROP TABLE IF EXISTS handovers;
ALTER TABLE bookings DROP CONSTRAINT IF EXISTS bookings_id_owner_uq;
