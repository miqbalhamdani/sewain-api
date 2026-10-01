-- Local use only. Migrations are forward-only in production -- see CLAUDE.md.
DROP TABLE IF EXISTS vehicle_unit_details;
DROP TABLE IF EXISTS vehicle_specs;
ALTER TABLE resource_units DROP CONSTRAINT IF EXISTS resource_units_id_owner_uq;
