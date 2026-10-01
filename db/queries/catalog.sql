-- Catalogue: resources and their physical units.  (S1-014 .. S1-017)
--
-- Every query here runs inside InOwnerTx, so RLS has already scoped both tables
-- to one rental. That is what makes the id-taking queries return zero rows for
-- another owner's id -- no ownership check in Go, and none wanted (BR-001).
--
-- owner_id appears only as an INSERT column value, where the policy's WITH CHECK
-- half validates it. It is never a WHERE clause.

-- name: ListResources :many
-- unit_count is active units only: a resource with none never appears in
-- availability search however healthy its own status looks (BR-010), and the
-- catalogue screen has to be able to say so.
SELECT r.id, r.name, r.category, r.pricing_unit, r.base_price,
       r.deposit_amount, r.late_fee_per_unit, r.min_duration, r.max_duration,
       r.buffer_minutes, r.requires_id_verification, r.status,
       r.description, r.terms_excludes, r.terms_requirements, r.terms_cancellation,
       v.vehicle_type, v.transmission, v.seats, v.fuel,
       (SELECT count(*) FROM resource_units u
         WHERE u.resource_id = r.id
           AND u.status = 'active' AND u.deleted_at IS NULL) AS unit_count
  FROM resources r
  LEFT JOIN vehicle_specs v ON v.resource_id = r.id
 WHERE r.deleted_at IS NULL
 ORDER BY r.name;

-- name: GetResource :one
SELECT r.id, r.name, r.category, r.pricing_unit, r.base_price,
       r.deposit_amount, r.late_fee_per_unit, r.min_duration, r.max_duration,
       r.buffer_minutes, r.requires_id_verification, r.status,
       r.description, r.terms_excludes, r.terms_requirements, r.terms_cancellation,
       v.vehicle_type, v.transmission, v.seats, v.fuel,
       (SELECT count(*) FROM resource_units u
         WHERE u.resource_id = r.id
           AND u.status = 'active' AND u.deleted_at IS NULL) AS unit_count
  FROM resources r
  LEFT JOIN vehicle_specs v ON v.resource_id = r.id
 WHERE r.id = $1 AND r.deleted_at IS NULL;

-- name: CreateResource :one
-- pricing_unit comes from the caller, but the caller is internal/catalog reading
-- owners.business_type -- never a request body (BR-012, BR-017). status is not
-- settable here: a resource is born active.
INSERT INTO resources (id, owner_id, created_by, name, category, pricing_unit,
                       base_price, deposit_amount, late_fee_per_unit,
                       min_duration, max_duration, buffer_minutes,
                       requires_id_verification, description, terms_excludes,
                       terms_requirements, terms_cancellation)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
RETURNING id;

-- name: UpdateResource :one
-- Two idioms in one statement, and the difference is the whole of BR-016.
--
-- COALESCE for the fields where absent means "leave it": there is no way to
-- clear them, so nil can stand in for "not mentioned".
--
-- CASE for the four nominals, where absent and null are DIFFERENT instructions:
-- absent leaves the value, null revokes it (04-api-spec.md section 3.2).
-- COALESCE cannot express that -- it reads both as "leave it" -- so each one
-- carries a companion boolean saying whether the key was there at all.
UPDATE resources SET
    name         = COALESCE(sqlc.narg(name),     name),
    category     = COALESCE(sqlc.narg(category), category),
    base_price   = COALESCE(sqlc.narg(base_price)::bigint, base_price),
    status       = COALESCE(sqlc.narg(status),   status),
    buffer_minutes = COALESCE(sqlc.narg(buffer_minutes)::int, buffer_minutes),
    requires_id_verification =
        COALESCE(sqlc.narg(requires_id_verification)::boolean, requires_id_verification),
    description        = COALESCE(sqlc.narg(description),        description),
    terms_excludes     = COALESCE(sqlc.narg(terms_excludes),     terms_excludes),
    terms_requirements = COALESCE(sqlc.narg(terms_requirements), terms_requirements),
    terms_cancellation = COALESCE(sqlc.narg(terms_cancellation), terms_cancellation),

    deposit_amount = CASE WHEN sqlc.arg(set_deposit_amount)::boolean
                          THEN sqlc.narg(deposit_amount)::bigint
                          ELSE deposit_amount END,
    late_fee_per_unit = CASE WHEN sqlc.arg(set_late_fee_per_unit)::boolean
                             THEN sqlc.narg(late_fee_per_unit)::bigint
                             ELSE late_fee_per_unit END,
    min_duration = CASE WHEN sqlc.arg(set_min_duration)::boolean
                        THEN sqlc.narg(min_duration)::int
                        ELSE min_duration END,
    max_duration = CASE WHEN sqlc.arg(set_max_duration)::boolean
                        THEN sqlc.narg(max_duration)::int
                        ELSE max_duration END,

    updated_at = now()
 WHERE id = sqlc.arg(id) AND deleted_at IS NULL
RETURNING id;

-- name: SoftDeleteResource :execrows
UPDATE resources SET deleted_at = now(), updated_at = now()
 WHERE id = $1 AND deleted_at IS NULL;

-- name: SoftDeleteUnitsOfResource :execrows
-- Runs in the same transaction as SoftDeleteResource. Units left behind would
-- belong to a kind nothing can see, and -- worse -- would keep holding their
-- codes, because the unique index only excludes deleted rows (BR-011). A plate
-- that moved to another car has to be usable on that car.
UPDATE resource_units SET deleted_at = now(), updated_at = now()
 WHERE resource_id = $1 AND deleted_at IS NULL;

-- name: ResourceExists :one
-- Distinguishes "already deleted" (204, idempotent) from "not in this rental"
-- (404). SoftDeleteResource alone cannot: both return zero rows.
SELECT EXISTS (SELECT 1 FROM resources WHERE id = $1);

-- name: ListUnits :many
SELECT u.id, u.resource_id, u.code, u.label, u.status, u.meter_value,
       u.condition_notes,
       d.year, d.color, d.tax_due_on, d.registration_valid_until
  FROM resource_units u
  LEFT JOIN vehicle_unit_details d ON d.resource_unit_id = u.id
 WHERE u.resource_id = $1 AND u.deleted_at IS NULL
 ORDER BY u.code;

-- name: CreateUnit :one
-- code is the client's (plate, serial -- BR-011), unlike bookings.code which the
-- server generates. status is not settable here: a unit is born active.
INSERT INTO resource_units (id, owner_id, created_by, resource_id, code, label,
                            meter_value, condition_notes)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING id;

-- name: UpdateUnit :one
-- No CASE pairs here: none of these four is a BR-016 nominal, so absent can
-- safely mean "leave it" and there is nothing to revoke.
UPDATE resource_units SET
    code            = COALESCE(sqlc.narg(code),   code),
    label           = COALESCE(sqlc.narg(label),  label),
    status          = COALESCE(sqlc.narg(status), status),
    meter_value     = COALESCE(sqlc.narg(meter_value)::bigint, meter_value),
    condition_notes = COALESCE(sqlc.narg(condition_notes), condition_notes),
    updated_at      = now()
 WHERE id = sqlc.arg(id) AND deleted_at IS NULL
RETURNING id;

-- name: GetUnit :one
SELECT u.id, u.resource_id, u.code, u.label, u.status, u.meter_value,
       u.condition_notes,
       d.year, d.color, d.tax_due_on, d.registration_valid_until
  FROM resource_units u
  LEFT JOIN vehicle_unit_details d ON d.resource_unit_id = u.id
 WHERE u.id = $1 AND u.deleted_at IS NULL;

-- name: SoftDeleteUnit :execrows
UPDATE resource_units SET deleted_at = now(), updated_at = now()
 WHERE id = $1 AND deleted_at IS NULL;

-- name: UnitExists :one
SELECT EXISTS (SELECT 1 FROM resource_units WHERE id = $1);

-- name: GetOwnerBusinessType :one
-- owners has no RLS -- it IS the tenant -- so this one really does need a WHERE.
-- BR-017: the preset picks the pricing unit for every resource in the rental,
-- and the mapping lives as a constant in Go, not as a column here.
SELECT business_type FROM owners WHERE id = $1;

-- ─────────────── BR-094 · atribut kendaraan, tabel pendamping ───────────────
--
-- Nol query di bawah punya `WHERE owner_id`. Kedua tabel ber-RLS seperti yang
-- lain, dan owner_id cuma muncul sebagai nilai kolom di INSERT -- tempat separuh
-- WITH CHECK policy memvalidasinya, dan tempat FK komposit memastikan ia cocok
-- dengan pemilik resource/unit induknya.

-- name: CreateVehicleSpec :exec
-- Dijalankan di transaksi yang sama dengan CreateResource. Kewajiban 1:1 dijaga
-- aplikasi karena database tidak bisa menegakkan "anak wajib ada" dengan murah;
-- yang dijaga database adalah kebalikannya -- anak tidak bisa menunjuk induk
-- pemilik lain.
INSERT INTO vehicle_specs (resource_id, owner_id, vehicle_type, transmission, seats, fuel)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: UpdateVehicleSpec :execrows
-- Mengganti seluruh objeknya, bukan COALESCE per field, dan itu disengaja:
-- `vehicle` di body adalah objek bersarang yang dirender form secara utuh, jadi
-- apa yang dikirim adalah apa yang seharusnya ada. `vehicle_type` tidak ikut --
-- ia dikunci sesudah resource dibuat (BR-094).
--
-- Konsekuensinya benar: mobil yang lupa mengirim `seats` melanggar
-- vehicle_specs_seats_car dan dijawab 422, bukan diam-diam kehilangan kursinya.
UPDATE vehicle_specs SET
    transmission = $2,
    seats        = $3,
    fuel         = $4,
    updated_at   = now()
 WHERE resource_id = $1;

-- name: CreateVehicleUnitDetail :exec
INSERT INTO vehicle_unit_details
    (resource_unit_id, owner_id, year, color, tax_due_on, registration_valid_until)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: UpdateVehicleUnitDetail :execrows
-- Mengganti seluruh objeknya, alasan yang sama dengan UpdateVehicleSpec. Di sini
-- ia juga yang membuat tanggal pajak salah ketik bisa DIKOSONGKAN lagi: COALESCE
-- akan membaca null sebagai "jangan sentuh", dan juragan tidak punya jalan
-- mencabutnya sama sekali.
UPDATE vehicle_unit_details SET
    year                     = $2,
    color                    = $3,
    tax_due_on               = $4,
    registration_valid_until = $5,
    updated_at               = now()
 WHERE resource_unit_id = $1;
