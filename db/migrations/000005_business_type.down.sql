ALTER TABLE owners DROP CONSTRAINT IF EXISTS owners_business_type_valid;
ALTER TABLE owners DROP COLUMN IF EXISTS business_type;
