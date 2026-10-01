-- Deskripsi dan syarat-ketentuan per resource.  (S1-085, BR-095)
--
-- `description` pernah ada di PRD section 6.2, dicabut di S1-014 dengan syarat yang
-- ditulis di 03-erd.md section 5: "Tambahkan kembali ketika ada layar yang
-- menampilkannya." Syarat itu sekarang terpenuhi dua kali -- form resource
-- mengisinya, halaman publik S1-060 merendernya. Kalau S1-060 batal, kolom ini
-- ikut dicabut lagi; itu bagian dari kesepakatannya.
--
-- Keempatnya tinggal di `resources`, bukan di tabel pendamping kendaraan. Kos,
-- lapangan, dan kamera juga punya syarat sewa dan kebijakan pembatalan; yang
-- membedakan cuma placeholder-nya, dan placeholder adalah konstanta di kode
-- (BR-017 aturan 4), bukan skema.
--
-- Deliberately NOT here: kolom untuk tenggat bayar, denda telat, atau toleransi
-- no-show dalam bentuk teks. Keempatnya SUDAH punya kolomnya sendiri, dan server
-- yang merakit kalimatnya. Juragan yang mengetik ulang "bayar maksimal 24 jam" ke
-- dalam textarea akan salah pada detik ia mengubah knob-nya, dan halaman publiknya
-- berbohong tanpa ada yang tahu (BR-095).

ALTER TABLE resources
    ADD COLUMN description       text,
    ADD COLUMN terms_excludes    text,
    ADD COLUMN terms_requirements text,
    ADD COLUMN terms_cancellation text;

-- Batasnya ditegakkan di sini karena halaman publik merendernya apa adanya: teks
-- 50 ribu karakter bukan syarat sewa, ia halaman yang rusak. Dinamai eksplisit
-- karena internal/catalog mencocokkan nama constraint, bukan pesan PostgreSQL.
ALTER TABLE resources
    ADD CONSTRAINT resources_description_length
        CHECK (description IS NULL OR char_length(description) <= 500),
    ADD CONSTRAINT resources_terms_excludes_length
        CHECK (terms_excludes IS NULL OR char_length(terms_excludes) <= 500),
    ADD CONSTRAINT resources_terms_requirements_length
        CHECK (terms_requirements IS NULL OR char_length(terms_requirements) <= 1000),
    ADD CONSTRAINT resources_terms_cancellation_length
        CHECK (terms_cancellation IS NULL OR char_length(terms_cancellation) <= 1000);
