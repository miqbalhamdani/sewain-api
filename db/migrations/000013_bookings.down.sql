-- Local use only. Migrations are forward-only in production -- see CLAUDE.md.
DROP TABLE IF EXISTS booking_counters;
DROP TABLE IF EXISTS bookings;
DROP FUNCTION IF EXISTS bookings_fill_end_at_with_buffer();
ALTER TABLE resource_units DROP CONSTRAINT IF EXISTS resource_units_id_resource_owner_uq;
