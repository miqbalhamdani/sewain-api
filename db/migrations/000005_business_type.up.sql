-- owners.business_type: the market preset, chosen once at registration.  (S1-082)
--
-- It is what decides pricing_unit for every resource this owner ever creates,
-- so the juragan is never asked for a price unit on an item form (BR-012,
-- BR-017). Asking would mean answering a question that only exists for a
-- vertical nobody has opened yet.

-- DEFAULT then DROP DEFAULT: the default exists only to fill rows that already
-- exist. New rows have to supply it, because there is no sensible guess -- a
-- rental of cameras priced as if it were a boarding house is wrong in a way
-- that stays wrong for every resource afterwards.
ALTER TABLE owners ADD COLUMN business_type text NOT NULL DEFAULT 'vehicle_rental';
ALTER TABLE owners ALTER COLUMN business_type DROP DEFAULT;

-- All six, not the two that phase 1 opens. The acceptance says "di luar ENAM
-- preset -> 422", and the schema knowing about a value is not the same as the
-- registration screen offering it: boarding_house and apartment are phase 2,
-- venue phase 3, clinic phase 4 (BR-017).
--
-- clinic's price unit is deliberately undecided -- PRD section 1 writes it as
-- "30 menit", which is not a value in BR-012's enum. Decided in phase 4, not
-- guessed now.
ALTER TABLE owners ADD CONSTRAINT owners_business_type_valid
    CHECK (business_type IN (
        'vehicle_rental','equipment_rental','boarding_house','apartment','venue','clinic'));
