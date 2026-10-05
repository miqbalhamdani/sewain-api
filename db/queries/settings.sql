-- name: GetSettings :one
-- Runs inside InOwnerTx like everything else, but owners has no RLS (it IS the
-- tenant), so the WHERE clause is what scopes it -- and the id comes from the
-- owner context, never from the request.
SELECT slug, booking_code_prefix, require_payment_before_pickup,
       draft_expiry_hours, payment_due_hours, no_show_tolerance_hours,
       notify_pickup_reminder, notify_return_reminder, notify_overdue_reminder,
       whatsapp, address, operating_hours,
       bank_name, bank_account_number, bank_account_holder, allowed_origins
  FROM owners
 WHERE id = $1;

-- name: UpdateSettings :one
-- COALESCE per column: a NULL argument means "the client omitted this key", so
-- the current value stands. That is what keeps PATCH from being a disguised PUT
-- that blanks every knob the caller did not happen to mention.
UPDATE owners SET
    slug                          = COALESCE(sqlc.narg(slug),                          slug),
    booking_code_prefix           = COALESCE(sqlc.narg(booking_code_prefix),           booking_code_prefix),
    require_payment_before_pickup = COALESCE(sqlc.narg(require_payment_before_pickup), require_payment_before_pickup),
    draft_expiry_hours            = COALESCE(sqlc.narg(draft_expiry_hours),            draft_expiry_hours),
    payment_due_hours             = COALESCE(sqlc.narg(payment_due_hours),             payment_due_hours),
    no_show_tolerance_hours       = COALESCE(sqlc.narg(no_show_tolerance_hours),       no_show_tolerance_hours),
    notify_pickup_reminder        = COALESCE(sqlc.narg(notify_pickup_reminder),        notify_pickup_reminder),
    notify_return_reminder        = COALESCE(sqlc.narg(notify_return_reminder),        notify_return_reminder),
    notify_overdue_reminder       = COALESCE(sqlc.narg(notify_overdue_reminder),       notify_overdue_reminder),
    whatsapp                      = COALESCE(sqlc.narg(whatsapp),                      whatsapp),
    address                       = COALESCE(sqlc.narg(address),                       address),
    operating_hours               = COALESCE(sqlc.narg(operating_hours),               operating_hours),
    bank_name                     = COALESCE(sqlc.narg(bank_name),                     bank_name),
    bank_account_number           = COALESCE(sqlc.narg(bank_account_number),           bank_account_number),
    bank_account_holder           = COALESCE(sqlc.narg(bank_account_holder),           bank_account_holder),
    allowed_origins               = COALESCE(sqlc.narg(allowed_origins)::text[],       allowed_origins),
    updated_at                    = now()
 WHERE id = sqlc.arg(id)
RETURNING slug, booking_code_prefix, require_payment_before_pickup,
          draft_expiry_hours, payment_due_hours, no_show_tolerance_hours,
          notify_pickup_reminder, notify_return_reminder, notify_overdue_reminder,
          whatsapp, address, operating_hours,
          bank_name, bank_account_number, bank_account_holder, allowed_origins;

-- name: GetOwnerProfile :one
-- What renters see of the business: the public page (BR-096) and the portal's
-- "cara bayar" (S1-062). owners has no RLS; the id comes from the Host lookup.
SELECT name, slug, whatsapp, address, operating_hours,
       bank_name, bank_account_number, bank_account_holder
  FROM owners
 WHERE id = $1;
