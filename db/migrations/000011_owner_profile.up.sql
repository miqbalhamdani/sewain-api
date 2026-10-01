-- Profil usaha yang dilihat penyewa.  (S1-086, BR-096)
--
-- Ini menambal janji yang sudah ada, bukan membuka fitur. ../docs/04-api-spec.md
-- section 4 mengirim { "owner": { "name": ..., "whatsapp": "+62..." } } di respons
-- katalog publik sejak sebelum kolomnya ada, dan acceptance S1-068 menuntut
-- "penyewa yang mendarat di katalog kosong harus tahu harus menghubungi siapa".
-- Dua janji, nol kolom di baliknya.
--
-- Ketiganya nullable, dan kosong bukan kasus pinggir: ia keadaan awal tiap usaha
-- yang baru mendaftar, persis seperti slug (BR-025, BR-005). Pendaftaran cuma
-- menanyakan empat hal. Yang berlaku: halaman publik tidak hidup sebelum slug,
-- whatsapp, dan address ketiganya terisi -- dan layar pengaturan (S1-066) wajib
-- mengatakan itu, bukan membiarkan juragan menebak kenapa halamannya kosong.

ALTER TABLE owners
    ADD COLUMN whatsapp        text,
    ADD COLUMN address         text,
    ADD COLUMN operating_hours text;

-- whatsapp satu-satunya dari ketiganya yang dibaca MESIN, bukan mata: halaman
-- publik menjadikannya tautan wa.me. Nomor berformat bebas menghasilkan tautan
-- mati di halaman yang seluruh gunanya menghubungi pemilik, dan itu lebih buruk
-- daripada tidak ada tombolnya sama sekali.
ALTER TABLE owners ADD CONSTRAINT owners_whatsapp_format
    CHECK (whatsapp IS NULL OR whatsapp ~ '^\+62[0-9]{8,13}$');

-- address dan operating_hours sengaja TANPA constraint. Yang pertama dibaca
-- manusia yang mau datang; yang kedua penuh pengecualian ("24 jam lewat WA",
-- "Minggu janjian dulu"), dan memaksanya jadi tujuh baris buka/tutup membuat
-- juragan mengisi data yang salah atau tidak mengisi sama sekali.
