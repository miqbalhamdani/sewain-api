-- Rekening transfer dan origin situs pemilik.  (M5: S1-062, S1-079)
--
-- Rekening: portal penyewa wajib menampilkan "instruksi transfer beserta nomor
-- rekening pemilik" (04-api-spec.md section 5, PRD D2), dan fase 1 tidak punya
-- gateway (BR-061). Tanpa kolomnya, penyewa harus bertanya lewat WhatsApp setiap
-- kali mau membayar. Ketiganya nullable karena itu keadaan awal tiap usaha, sama
-- seperti profil di 000011; portal jatuh ke tombol WhatsApp selama kosong.
--
-- allowed_origins: ditunda dari 000004 sampai S1-079 memilikinya. Ia ada di sini,
-- bukan di migrasi api_keys, karena ia knob PATCH /settings -- dan kontrak Settings
-- mewajibkannya di setiap respons sejak kontrak M5 mendarat.

ALTER TABLE owners
    ADD COLUMN bank_name           text,
    ADD COLUMN bank_account_number text,
    ADD COLUMN bank_account_holder text,
    ADD COLUMN allowed_origins     text[] NOT NULL DEFAULT '{}';

-- Nomor yang disalin penyewa ke aplikasi bank: angka saja. Spasi dan titik dari
-- juragan berakhir sebagai transfer yang gagal, jadi database yang menolaknya.
ALTER TABLE owners ADD CONSTRAINT owners_bank_account_number_format
    CHECK (bank_account_number IS NULL OR bank_account_number ~ '^[0-9]{5,20}$');
