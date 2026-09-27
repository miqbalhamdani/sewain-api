ALTER TABLE owners
    DROP CONSTRAINT IF EXISTS owners_no_show_tolerance_non_negative,
    DROP CONSTRAINT IF EXISTS owners_payment_due_hours_positive,
    DROP CONSTRAINT IF EXISTS owners_booking_code_prefix_format;

ALTER TABLE owners
    DROP COLUMN IF EXISTS no_show_tolerance_hours,
    DROP COLUMN IF EXISTS payment_due_hours,
    DROP COLUMN IF EXISTS booking_code_prefix;
