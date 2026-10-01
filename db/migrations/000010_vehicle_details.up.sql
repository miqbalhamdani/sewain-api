-- Atribut kendaraan: dua tabel pendamping 1:1.  (S1-085, BR-094)
--
-- Transcribed from ../docs/03-erd.md sections 1 and 3. Where this file and those
-- sections disagree, this file is the bug.
--
-- Kenapa tabel pendamping, dan bukan dua alternatif yang lebih gampang:
--
--   Kolom di `resources`   -- kamera, kos, dan lapangan ikut mewarisi
--     `transmission` yang selamanya kosong. `equipment_rental` sama-sama dibuka
--     di fase 1, jadi ini bukan masalah teoretis. Mode gagalnya persis yang
--     dipakai BR-017 aturan 2 untuk menolak pemilih satuan harga.
--
--   `jsonb`                -- ketiga CHECK lintas kolom di bawah tidak bisa
--     ditulis sama sekali, dan ketiganya adalah seluruh alasan tabel ini ada.
--     Semangat yang sama dengan BR-012: mendaftarkan nilai yang sah tanpa
--     menegakkannya sama dengan tidak punya aturan.

-- Target FK komposit untuk vehicle_unit_details. Alasannya sama persis dengan
-- resources_id_owner_uq di 000007: cek foreign key berjalan sebagai pemilik tabel
-- dan MELEWATI row level security, jadi FK biasa ke resource_units(id) menerima id
-- pemilik mana pun. id sendiri sudah PK; ini ada supaya pasangannya punya target.
ALTER TABLE resource_units ADD CONSTRAINT resource_units_id_owner_uq
    UNIQUE (id, owner_id);

-- ---------------------------------------------------------------------------
-- vehicle_specs -- satu baris per resource
-- ---------------------------------------------------------------------------

CREATE TABLE vehicle_specs (
    -- Sekaligus PK dan separuh FK komposit: 1:1 ditegakkan bentuk kuncinya, bukan
    -- oleh sebuah unique index tambahan.
    resource_id uuid PRIMARY KEY,
    owner_id    uuid NOT NULL,

    vehicle_type text NOT NULL,
    transmission text NOT NULL,
    seats        int,
    fuel         text NOT NULL,

    -- ON DELETE CASCADE di bawah adalah satu-satunya tempat di skema ini yang
    -- memakainya. resources di-soft-delete lewat deleted_at, jadi cascade tidak
    -- pernah menyala dari jalur API; ia ada untuk jalur yang benar-benar menghapus
    -- baris -- pembersihan manual, test, pemulihan. Spek tanpa resource-nya bukan
    -- data, ia sampah yang masih memegang kunci komposit.
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Dinamai eksplisit, bukan dibiarkan di dalam CREATE TABLE: FK anonim diberi nama
-- oleh PostgreSQL, dan internal/catalog mencocokkan NAMA untuk menerjemahkan
-- pelanggarannya jadi 404. Menebak nama yang dipilih PostgreSQL adalah cara M1
-- menghasilkan 500 di tempat kontrak menjanjikan 422.
ALTER TABLE vehicle_specs
    ADD CONSTRAINT vehicle_specs_resource_matches_owner
    FOREIGN KEY (resource_id, owner_id) REFERENCES resources (id, owner_id)
    ON DELETE CASCADE;

ALTER TABLE vehicle_specs
    ADD CONSTRAINT vehicle_specs_type_valid
        CHECK (vehicle_type IN ('car','motorcycle')),
    ADD CONSTRAINT vehicle_specs_transmission_valid
        CHECK (transmission IN ('manual','automatic','clutch')),
    ADD CONSTRAINT vehicle_specs_fuel_valid
        CHECK (fuel IN ('gasoline','diesel','hybrid','electric')),
    ADD CONSTRAINT vehicle_specs_seats_range
        CHECK (seats IS NULL OR seats BETWEEN 2 AND 20);

-- Tiga aturan lintas kolom. Yang pertama ditulis sebagai KESETARAAN, bukan dua OR,
-- dan itu bukan gaya penulisan: ia menegakkan dua arah sekaligus -- mobil WAJIB
-- punya jumlah kursi, motor DILARANG punya. Versi "mobil wajib punya kursi" saja
-- akan menerima motor berkursi empat tanpa satu pun test jadi merah.
ALTER TABLE vehicle_specs
    ADD CONSTRAINT vehicle_specs_seats_car
        CHECK ((vehicle_type = 'car') = (seats IS NOT NULL)),
    ADD CONSTRAINT vehicle_specs_clutch_moto
        CHECK (transmission <> 'clutch' OR vehicle_type = 'motorcycle'),
    ADD CONSTRAINT vehicle_specs_diesel_car
        CHECK (fuel <> 'diesel' OR vehicle_type = 'car');

SELECT enable_owner_rls('vehicle_specs');

-- ---------------------------------------------------------------------------
-- vehicle_unit_details -- satu baris per unit fisik
-- ---------------------------------------------------------------------------

CREATE TABLE vehicle_unit_details (
    resource_unit_id uuid PRIMARY KEY,
    owner_id         uuid NOT NULL,

    year  int NOT NULL,
    color text,

    -- Dua tanggal yang TIDAK PERNAH keluar ke permukaan publik. `code` adalah plat
    -- nomor dan BR-025 sudah melarangnya keluar; pajak dan STNK adalah catatan
    -- juragan untuk armadanya sendiri. Pengingat otomatisnya belum ada -- kolomnya
    -- disiapkan, notifikasinya tidak (docs/ideas/vehicle-attributes.md).
    tax_due_on               date,
    registration_valid_until date,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE vehicle_unit_details
    ADD CONSTRAINT vehicle_unit_details_unit_matches_owner
    FOREIGN KEY (resource_unit_id, owner_id) REFERENCES resource_units (id, owner_id)
    ON DELETE CASCADE;

-- Batas atasnya sengaja longgar sampai 2100. "Tahun depan" adalah batas yang benar
-- dan ia tidak bisa jadi CHECK: predikatnya butuh now(), sedangkan CHECK wajib
-- IMMUTABLE. Aplikasi yang menyempitkannya; database yang menolak yang absurd.
ALTER TABLE vehicle_unit_details
    ADD CONSTRAINT vehicle_unit_details_year_range
        CHECK (year BETWEEN 1990 AND 2100);

SELECT enable_owner_rls('vehicle_unit_details');
