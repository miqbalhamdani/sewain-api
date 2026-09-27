# sewain-api — Go API & worker

Fase 1 · Rental & sewa. Go 1.26 · PostgreSQL 18 (`btree_gist` wajib) · Redis 8 ·
Cloudflare R2 · WhatsApp Cloud API.

**Kontraknya ada di `../docs/`.** Baca sebelum mengubah apa pun yang dicakupnya:

| File | Otoritatif untuk |
|---|---|
| `01-product-requirements.md` | Kenapa fase ini ada, ruang lingkup, state machine |
| `02-business-rules.md` | Aturan bisnis bernomor `BR-xxx`. **Rujukan tertinggi** — kalau dokumen lain berbeda, ini yang menang |
| `03-erd.md` | Bentuk database: tabel, index, constraint |
| `04-api-spec.md` | Kontrak HTTP dalam prosa |
| `openapi.yaml` | Kontrak yang sama, terbaca mesin — sumber codegen dua repo |

Kalau kode dan kontrak berbeda, **kodenya yang salah** — kecuali kontraknya yang salah, dan itu
diubah di sana dulu, di PR-nya sendiri.

Backlog fase ini: [`../docs/05-backlog.md`](../docs/05-backlog.md) — **satu backlog untuk dua repo**,
berurut `S1-001`–`S1-077`. Item BE mendahului layar FE-nya di milestone yang sama; sebuah
milestone baru selesai ketika layarnya ikut selesai. Ambil item `todo` bernomor terkecil yang
dependensinya sudah `done`.

---

## Perintah

```bash
make dev          # jalankan API terhadap PostgreSQL & Redis di host
make db-create    # bikin database development lokal
make migrate      # terapkan migrasi
make generate     # sqlc + oapi-codegen. WAJIB no-op di tree yang bersih
make test         # unit + integrasi terhadap PostgreSQL host; database scratch per run
make test-iso     # suite isolasi owner atas seluruh route terdaftar
make test-race    # test konkurensi booking. Wajib hijau, lihat BR-022
make lint         # golangci-lint
make lint-imports # hanya internal/db yang boleh buka koneksi
make lint-rls     # gagal kalau ada tabel ber-owner_id tanpa policy RLS
make check        # generate + lint + lint-imports + lint-rls + test + test-race
```

`make check` hijau adalah syarat PR. CI menjalankan persis daftar ini.

---

## Layout

```
cmd/api/          HTTP API. Stateless.
cmd/worker/       Konsumer antrean: scan bukti transfer, derivatif foto, pengingat WA
cmd/scheduler/    Job berjadwal: draft kedaluwarsa, pengingat H-1
cmd/migrate/      Jalankan migrasi sampai selesai, lalu keluar
internal/
  platform/       config, logging, tracing, errors — diimpor semua paket
  db/             pool, InOwnerTx, output sqlc. SATU-SATUNYA paket yang impor pgx
  owner/          konteks owner per-request
  auth/           JWT, argon2id, RBAC owner|operator
  catalog/        resources, resource_units
  booking/        ketersediaan, booking, state machine
  handover/       bukti kondisi (append-only)
  billing/        invoices, invoice_lines, deposit, denda
  payment/        bukti transfer + pelunasan manual. Gateway & webhook-nya NONAKTIF di fase 1
  notify/         WhatsApp Cloud API
  report/         laporan
  storage/        S3-compatible: R2 di produksi, MinIO di lokal. Presign PUT+GET, HEAD, copy; prefix per owner
  http/           router chi, middleware, handler, DTO
db/migrations/    golang-migrate, SQL polos, up + down
```

### Aturan impor (ditegakkan `make lint-imports`)

- **Hanya `internal/db` yang membuka koneksi** — ia satu-satunya yang boleh mengimpor
  `pgxpool`. Itu yang dicek `make lint-imports`, dan ia terbukti merah kalau dilanggar.

  Yang **tidak** dicek: `pgx` telanjang. `pgx.Tx` muncul di signature callback `InOwnerTx`,
  jadi setiap paket yang menulis apa pun ikut menyebutnya — melintnya berarti gagal selamanya
  atau mengecualikan semua orang. Yang benar-benar dijaga adalah kepemilikan koneksi, dan itu
  `pgxpool`. Satu aturan tidak butuh `go-arch-lint`; empat baris `awk` di `Makefile` cukup.
- Paket domain tidak saling mengimpor. Mereka dirakit di `internal/http`, atau lewat interface
  yang dideklarasikan konsumennya.
- Tidak ada yang mengimpor `internal/http`.

---

## Isolasi owner — aturan yang paling penting

Setiap tabel bertenant punya `owner_id`, `ENABLE ROW LEVEL SECURITY` **dan** `FORCE ROW LEVEL
SECURITY`. `FORCE` yang menentukan: tanpanya, pemilik tabel melewati policy-nya sendiri — dan
peran itulah yang dipakai migrasi. Aplikasi konek sebagai `app_user`, yang tidak memiliki apa pun.

> Dibuktikan, bukan diasumsikan: `../docs/03-verify-constraints.sql` bagian RLS menjalankan
> enam kasus dari **peran non-superuser** — superuser melewati RLS sepenuhnya, `FORCE` sekalipun.
>
> **Ini naik satu tingkat dari `boarding-house-api`,** yang menegakkan BR-001 lewat filter di
> setiap query. Filter di query benar sampai ada satu query yang lupa, dan yang lupa itu tidak
> gagal — ia mengembalikan data pemilik lain. RLS membuat kegagalannya jadi baris kosong, bukan
> kebocoran.

**Setiap query lewat `InOwnerTx`.** Owner di-set per *transaksi* dengan `set_config(..., true)`,
tidak pernah per koneksi — pgx memakai pool, dan `SET` yang bocor ikut ke request owner berikutnya.

```go
err := store.InOwnerTx(ctx, func(tx pgx.Tx) error {
    return q.WithTx(tx).CreateBooking(ctx, params)
})
```

**Gagal tertutup.** Tanpan owner di context → `ErrNoOwnerContext`. Jangan dibiarkan lolos: tanpa
owner, `current_setting('app.owner_id', true)` bernilai `NULL`, policy jadi `NULL`, dan semua baris
tersaring — aman, tapi tampil sebagai hasil kosong yang membingungkan, bukan sebagai bug.

**Dua jalur yang tidak punya token**, dan keduanya tetap tidak menerima `owner_id` dari luar:

1. **Halaman publik** — `owner_id` diturunkan dari header `Host` (`rentalbudi.sewain.id` → slug
   `rentalbudi`), di satu middleware, lalu masuk ke `InOwnerTx` seperti request bertoken (BR-030).
   Slug **tidak ada di path**.
2. **Portal penyewa** — dari klaim token link, dibatasi ke satu `booking_id` (BR-002).

Menambah jalur ketiga adalah perubahan skema, perubahan kontrak, dan sebuah percakapan.

**Index komposit diawali `owner_id`.** RLS menambahkan predikat itu ke setiap query; index yang
tidak bisa melayaninya cuma beban.

### `owner` berarti dua hal, dan keduanya benar

Tidak bisa dihindari, jadi lebih baik dinamai: **`owner` adalah entitas tenant-nya**
(tabel `owners`, kolom `owner_id`, paket `internal/owner`) **dan sekaligus salah satu
dari dua peran** (`users.role IN ('owner','operator')`, `auth.RoleOwner`). Keduanya
sudah dikunci kontrak — BR-003 untuk peran, BR-025 untuk entitas.

Dalam kode keduanya tidak pernah bertabrakan karena beda paket: `owner.FromContext(ctx)`
mengembalikan **usaha**-nya; `auth.RoleOwner` adalah **peran** di usaha itu. Yang
bertabrakan cuma kalimat bahasa Inggris. Kalau ragu, tulis "rental" untuk entitas.

Satu user milik tepat satu usaha (BR-004), jadi `owner_id` dan `role` sama-sama datang
dari satu baris `users` dan sama-sama masuk klaim token. Tidak ada usaha "aktif" yang
bisa berubah di tengah sesi, dan tidak ada endpoint untuk menggantinya.

---

## Anti double-booking — alasan produk ini ada

`../docs/03-erd.md` §3 memuat DDL-nya, dan dua skrip membuktikannya **tanpa satu baris Go** — di
database scratch, `ROLLBACK`, tidak meninggalkan apa pun:

| Skrip | Isi |
|---|---|
| `../docs/03-verify-overlap-constraint.sql` | 6 kasus anti-bentrok + trigger buffer (BR-015, BR-022, BR-023) |
| `../docs/03-verify-constraints.sql` | 43 kasus constraint di `03-erd.md` §3, plus 6 kasus RLS |
| `../docs/03-verify-with-check.sql` | Bukti diferensial `WITH CHECK`: `USING` saja bocor saat **menulis** |

Jalankan **sebelum** menulis migrasinya. Ini bukan formalitas: versi `end_at_with_buffer` sebagai
kolom generated lolos dua kali audit-baca lalu ditolak PostgreSQL dalam dua detik
(`generation expression is not immutable`). Membaca dokumen tidak menemukan hal seperti itu.

Yang perlu dipegang saat ngoding:

**Kebenarannya dijaga `bookings_no_overlap`, bukan oleh kode.** Aplikasi tetap mengecek lebih dulu
supaya pesan errornya bisa menyebut booking yang bentrok — tapi cek itu **kenyamanan**, bukan
penegakan. Dua request bersamaan aman tanpa kunci di level aplikasi.

```go
// Pelanggaran constraint diterjemahkan jadi error domain, bukan bocor sebagai 500.
// Pola: mapAssignmentUnique di boarding-house-api/internal/repository/onboarding_repository.go
if pgErr.ConstraintName == "bookings_no_overlap" {
    return domain.ErrBookingConflict
}
```

- **`end_at_with_buffer` diisi database lewat trigger** `bookings_end_at_with_buffer`. Jangan
  menghitungnya di Go dan jangan mengirimnya di `INSERT` — satu jalur insert yang lupa
  menghasilkan buffer `0`, dan constraint tetap lolos: tidak bentrok, tapi tanpa jeda
  bersih-bersih. Gagalnya senyap. Kalau tetap dikirim, trigger **menimpanya**.

  Trigger, bukan kolom generated: `GENERATED ALWAYS AS (end_at + make_interval(...))` ditolak
  PostgreSQL — `generation expression is not immutable`, karena `timestamptz + interval` ditandai
  STABLE demi DST. Terverifikasi lewat `../docs/03-verify-overlap-constraint.sql`; jangan
  "optimasi" balik ke kolom generated.

  Yang **tetap** tugas server: men-snapshot `buffer_minutes` dari `resources` ke booking, bukan
  membacanya ulang belakangan (BR-014, BR-015).
- **Rentang `[)`** di seluruh sistem: berakhir 10:00 dan mulai 10:00 tidak bentrok (BR-022).
- **Hanya `reserved` dan `picked_up` yang mengunci.** `draft` dari halaman publik sengaja tidak
  memblokir penjualan sungguhan (BR-023, BR-026).
- **`draft → reserved` menjalankan ulang cek bentrok.** Konfirmasi boleh gagal; itu benar (BR-026).

**Jangan pernah menambahkan lock aplikasi, advisory lock, atau antrean serialisasi di jalur ini.**
Kalau terasa perlu, yang sebenarnya terjadi adalah constraint-nya dilewati di suatu tempat.

---

## Database

- **Migrasi forward-only di produksi.** Tulis `down` untuk lokal; jangan pernah mengandalkannya.
- **Tabel ber-owner baru memanggil `SELECT enable_owner_rls('<tabel>');` di migrasinya sendiri.**
  Itu seluruh setup tenancy untuk satu tabel — fungsi itu melakukan `ENABLE`, `FORCE`, dan policy
  `owner_isolation`, dan menolak tabel tanpa `owner_id`. Jangan tulis tangan keempat statement-nya;
  mode gagal dari `FORCE` yang hilang itu senyap.
- **`sqlc`, bukan ORM.** RLS, `FOR UPDATE`, exclusion constraint, dan partial index butuh SQL yang
  kamu kendalikan. Query di `db/queries/*.sql`; output `internal/db/sqlcgen/`, tidak pernah diedit.
- **Key UUID v7**, dibuat di Go (`uuid.NewV7`). Jangan `gen_random_uuid()` di insert aplikasi.
- **Uang `bigint` rupiah penuh.** `120000` = Rp 120.000. Bukan sen, bukan float, bukan `numeric`.
  Ini **beda dari `new-commerce`** yang memakai minor unit — jangan salin helper uangnya bulat-bulat.
- **Waktu `timestamptz`, selalu UTC.** Tampilan Asia/Jakarta urusan frontend.
- **Soft delete `deleted_at`**, kecuali tabel bukti (`handovers`, `handover_photos`,
  `payment_gateway_transactions`, `audit_logs`).
- **Tabel ber-`owner_id` tanpa policy RLS bikin CI merah.** Jangan matikan cek itu. `make lint-rls`
  membaca database yang hidup, bukan file migrasi — migrasi yang benar tapi tak pernah diterapkan
  lolos cek berbasis sumber dan tetap meninggalkan tabel terbuka.

### Kolom yang tidak ada, dan tidak boleh ditambahkan

| Bukan kolom | Dapatnya dari | BR |
|---|---|---|
| `invoices.total` | `SUM(invoice_lines.amount)` saat dibaca | BR-055 |
| Status `overdue` | `status = 'picked_up' AND end_at < now()` | BR-041 |
| `resource_units.status = 'rented'` | Turunan dari booking `picked_up` | BR-023 |
| Cache ketersediaan halaman publik | Dihitung dari `bookings` tiap kali diminta | BR-025 |

Empat baris ini alasan tidak ada satu pun cron yang mengubah data di sistem ini. Scheduler yang
ada cuma dua, dan keduanya mengubah hal yang memang berubah: **kedaluwarsa** (draft BR-027,
tenggat bayar BR-057, `no_show`) dan **pengingat** (BR-070). Retensi identitas (BR-086) ditunda —
tidak ada job penghapus di fase 1.

Perhatikan yang **tidak** dilakukan job kedaluwarsa: ia tidak pernah menyentuh booking yang sudah
`picked_up`, dan tidak membatalkan apa pun kalau `require_payment_before_pickup` mati — pemilik itu
memang menerima pembayaran saat pengambilan (BR-038, BR-057).

---

## HTTP

- Interface server digenerate dari `../docs/openapi.yaml` lewat `oapi-codegen`. Jangan tulis
  tangan registrasi route untuk endpoint yang sudah terdokumentasi.
- **`trace_id` wajib bisa ditelusuri ke span-nya** (BR-092). Ia diambil dari konteks OpenTelemetry, bukan
  UUID yang dibuat di tempat. `trace_id` yang tidak menunjuk ke span apa pun cuma string acak yang
  membuat orang mengira punya observability — dan frontend memang menyuruh pengguna menyalinnya ke
  tiket support. Span memuat `owner_id`, route, dan durasi query.
- **Error RFC 9457** `application/problem+json` + `trace_id`. Pakai `platform/errors`; jangan
  `http.Error` di handler. Katalog `type` ada di `../docs/04-api-spec.md` §2.
- **Cek izin di batas handler**, sebagai pembungkus method yang digenerate. Bukan di service,
  bukan di query: makin dalam, makin banyak jalur panggilan yang harus ingat, dan yang lupa jadi
  lubang otorisasi senyap, bukan error kompilasi.
- **`internal/auth/roles.go` satu-satunya definisi matriks peran** (BR-003). Handler, respons
  login, dan `GET /me` membacanya dari sana supaya tidak bisa berbeda.
- **Field yang dikelola server diabaikan saat create dan `422` saat update**: `id`, `owner_id`,
  `code`, `end_at_with_buffer`, seluruh kolom snapshot harga, `actual_return_at`, `created_at`.
- **Kosong ≠ null.** Key yang absen memakai default; `null` eksplisit adalah error validasi.
- **Paginasi cursor saja.** Tanpa `offset`.
- **`Idempotency-Key` ditangani satu middleware, bukan per handler** (BR-090). Kunci → Redis, TTL 24 jam,
  respons pertama disimpan dan diputar ulang bulat-bulat. Kunci yang masih berjalan dijawab
  `409 request-in-flight`, bukan dijalankan paralel. `../docs/04-api-spec.md` §2.1 punya daftar
  endpoint yang mewajibkannya.

  Constraint database sudah mencegah kerusakan datanya; yang diperbaiki middleware ini adalah
  responsnya. Submit ganda yang dijawab `booking-conflict` menyalahkan operator atas timeout
  jaringan yang bukan salahnya.
- **Owner dari token, atau dari `Host`.** Tidak pernah dari path, query, atau body.

  `Host` memang sebuah header, dan BR-001 melarang menurunkan owner dari header — pengecualian ini
  disengaja dan punya syarat. **`Host` bukan data aplikasi, ia amplop routing:** proxy sudah memilih
  sertifikat lewat SNI dan menolak host yang tidak dikonfigurasi, jadi ia tersaring sebelum jadi
  input. Yang membuatnya benar bukan header-nya, tapi topologinya — lihat larangan terakhir di
  §Jangan pernah.

### Permukaan publik — `<slug>.sewain.id` (BR-030)

Satu middleware, tiga kewajiban, jangan sampai ada yang lolos satu pun:

1. Resolve `Host → owner_id`, atau `404`. Pemilik `suspended` juga `404` — **respons yang sama
   persis** dengan host asing dan dengan resource milik pemilik lain. Selisih respons antar kasus
   adalah cara katalog pemilik lain bocor.
2. Rate limit per IP **dan** per pemilik. Keduanya, karena satu IP menyerang banyak pemilik dan
   banyak IP menyerang satu pemilik adalah dua serangan yang berbeda.
3. Hanya-baca kecuali `POST /public/bookings`.

Peta host-nya, supaya tidak ada yang menaruh endpoint di tempat yang salah:

```
app.sewain.id          backoffice + API-nya                        JWT
api.sewain.id          webhook + API eksternal (BR-031, BR-032)    tanda tangan / X-API-Key
<slug>.sewain.id       publik + portal penyewa /booking/<token>    Host
```

Respons publik tidak pernah memuat `resource_units.code`, id unit, nama penyewa, atau `owner_id`
(BR-025). Ini diuji, bukan diingat: `make test-iso` punya kasus khusus permukaan publik.

---

## Pembayaran fase 1: manual saja (BR-061)

**Jalur payment gateway dimatikan.** `../docs/04-api-spec.md` §3.8.1 memuat keputusan lengkapnya.
Yang perlu dipegang saat ngoding:

- **Tiga endpoint tidak didaftarkan:** `POST /invoices/{id}/payment-link`,
  `POST /portal/bookings/{token}/payment-link`, dan `POST /webhooks/payments/{provider}`. Tidak
  masuk `openapi.yaml`, jadi tidak ada method-nya di kode generated. `404`, bukan `503` — endpoint
  yang ada tapi selalu gagal akan digenerate lalu dipanggil orang.
- **Yang aktif:** `POST /invoices/{id}/payments` (transfer tercek & tunai), `POST
  /invoices/{id}/proofs`, `POST /proofs/{id}/approve`. Pelunasan **selalu** lewat persetujuan
  manusia (BR-062) — tidak ada jalur otomatis apa pun.
- **`invoices.status = 'gateway_pending'` dan `payments.method = 'gateway'` tidak terjangkau.**
  Enum-nya tetap ada supaya menyalakan kembali tidak butuh migrasi. **Jangan menulis test yang
  menunggu status itu muncul.**
- **`payment_gateway_transactions` tetap ada dan tetap kosong.** Bukan tabel mati; tabel yang
  belum kebagian baris.
- **BR-063 dan BR-064 menganggur, bukan batal.** Keduanya kontrak yang berlaku begitu gateway
  kembali — jangan dihapus, dan jangan dirancang ulang nanti.

Menyalakan kembali = daftarkan tiga endpoint itu. Nol perubahan skema, nol migrasi.

---

## Bukti kondisi bersifat append-only

`handovers` dan `handover_photos` **tidak punya** endpoint `PATCH` maupun `DELETE`, tidak punya
`deleted_at`, dan tidak punya query update di `db/queries/`. Percobaan mengubahnya dijawab
`405 evidence-immutable` (BR-037).

Koreksi dilakukan dengan menambah baris catatan baru. Nilai seluruh fitur ini sebagai penyelesai
sengketa hilang total begitu ia bisa diedit belakangan.

`actual_return_at` diisi jam server, bukan dari body (BR-040). Foto minimal 1, ditolak `422` kalau
kosong — bukan peringatan, bukan opsional (BR-036).

---

## Object storage: R2, dan kenapa off-box

Satu adapter S3-compatible, dua target: **MinIO di lokal, Cloudflare R2 di produksi.** Kodenya
identik; yang beda cuma endpoint dan kredensial.

- **Object storage tidak pernah tinggal di mesin aplikasi.** Foto serah-terima adalah
  satu-satunya alasan sengketa deposit bisa diselesaikan (PRD §2.2), dan BR-037 melarang siapa pun
  menghapusnya. Kalau ia hilang bersama VPS-nya, jaminan itu bohong — dan tidak ada tempat lain
  untuk memulihkannya. Karena itu `S1-074` sengaja **tidak** memuat object storage, dan `pgBackRest`
  juga menulis ke R2, bukan ke disk mesin yang sama.
- **Simpan kunci objek, bukan URL.** URL memuat endpoint, region, dan kadang tanda tangan; semuanya
  berubah. Kunci tidak.
- **Prefiks setiap kunci dengan `owner_id`.** Satu kunci yang bocor lewat log atau screenshot tidak
  boleh membuka jalan menebak objek pemilik lain.
- **TTL presigned GET berbeda per jenis** (`S1-033`): foto serah-terima 1 jam, berkas ekspor
  15 menit (BR-077), foto identitas **5 menit** (BR-085) — yang terakhir dibuka sekali lalu ditutup,
  dan setiap pembukaannya menulis baris audit.
- **Byte unggahan tidak pernah lewat API** — presigned PUT dari browser langsung ke R2 (BR-093),
  sama seperti `new-commerce`. Yang menjaga BR-036/BR-037 bukan lewatnya byte, tapi dua hal lain:
  **server yang menentukan kunci**, dan **`HEAD` sebelum menulis baris**. Objek yang ada tanpa
  barisnya cuma sampah — tidak ada yang menunjuknya, tidak ada yang bisa membacanya, dan lifecycle
  `pending/` 24 jam yang mengurusnya. Yang berbahaya justru baris yang menunjuk objek karangan,
  dan `HEAD` itu yang menutupnya.
- **Unggahan mendarat di `pending/<owner_id>/…`, lalu disalin ke prefiks final saat commit.**
  Salinannya R2→R2, nol egress lewat aplikasi. Prefiks bukti hanya berisi bukti yang sudah punya
  baris.

---

## Job runner (BR-091)

`cmd/worker` mengonsumsi Redis Streams; `cmd/scheduler` menaruh pekerjaan berjadwal ke stream
yang sama. Keduanya berdiri di `S1-040`, dan **lima item sesudahnya memakainya apa adanya** —
scan bukti transfer (`S1-046`), draft kedaluwarsa (`S1-052`), pengingat WA (`S1-054`), ekspor
laporan (`S1-057`).

- **Pengiriman at-least-once, jadi handler wajib idempoten.** Ini idempotensi sebagai sifat
  desain, bukan lewat header — beda mekanisme dari BR-090. Satu pekerjaan bisa jalan dua kali
  saat worker mati di tengah. Handler yang tidak tahan itu akan mengirim dua pengingat WhatsApp ke
  penyewa yang sama — dan BR-071 ada justru supaya itu tidak terjadi.
- **Gagal → retry dengan backoff → dead-letter.** Tidak pernah hilang diam-diam, tidak pernah
  retry selamanya. Isi dead-letter tampil di dashboard, bukan cuma di log (BR-072).
- **Scheduler tidak boleh jalan dobel.** Dua replika berarti dua kali kirim; pakai kunci lease di
  Redis, bukan asumsi bahwa cuma ada satu proses.
- **Jangan gabungkan keduanya jadi satu biner.** Ekspor XLSX dan scan AI rakus memori dan
  bergantung layanan luar; draft kedaluwarsa dan pengingat nyaris nol sumber daya. Batas sumber
  daya per layanan tidak bisa dipasang ke satu biner yang memuat dua profil itu, dan satu ekspor
  yang kehabisan memori tidak boleh ikut menjatuhkan pengingat (BR-091).
- **Jangan pernah `time.Ticker` di dalam `cmd/api`.** Ia jalan sekali per replika, mati saat
  deploy, dan tidak punya jejak kalau gagal. Kalau sebuah task butuh pekerjaan berjadwal,
  tempatnya `cmd/scheduler`.
- **Status pekerjaan hidup di Redis, bukan tabel** — kecuali `notifications`, yang wajib durabel
  karena BR-072. Lihat `../docs/03-erd.md` §4.

---

## Testing

| Lapis | Cara |
|---|---|
| Unit | Logika murni: hitung durasi, denda telat, sisa deposit, matriks izin |
| Integrasi | PostgreSQL asli **di host**, database scratch dibuat & dibuang tiap run — nol docker. **Jangan mock database** — RLS, trigger, dan exclusion constraint tidak bisa dimock secara bermakna |
| Isolasi | `make test-iso`: duan owner ter-seed, seluruh route terdaftar, nol kebocoran |
| **Konkurensi** | `make test-race`: dua insert bersamaan untuk unit & rentang yang sama, tepat satu berhasil |
| Migrasi | Setiap migrasi diterapkan ke snapshot yang dipulihkan, di CI |

Pola scratch-database itu bukan hal baru: `../docs/03-verify-overlap-constraint.sql` dan
`../docs/03-verify-constraints.sql` sudah memakainya dan sudah hijau di mesin ini. `make test`
cuma menaikkannya jadi strategi test, bukan menciptakan pola.

**Route baru di `openapi.yaml` butuh kasus isolasinya di PR yang sama.** `make test-iso`
menelusuri route yang didaftarkan kode generated dan gagal untuk yang tidak punya entri.

**Test konkurensi BR-022 bukan opsional dan bukan sekali jalan.** Ia hidup di `make check` selamanya.
Ini satu-satunya test yang membuktikan klaim utama produk ini, dan satu-satunya yang gagal kalau
seseorang "mengoptimasi" constraint-nya jadi cek aplikasi.

---

## Jangan pernah

- `pool.Query` di luar `internal/db`.
- Menerima `owner_id`, `unit_price`, `pricing_unit`, `deposit_amount`, atau `late_fee_per_unit`
  dari request body. `pricing_unit` diisi dari `owners.business_type` saat resource dibuat (BR-017).
- Membaca ulang harga dari `resources` untuk booking yang sudah ada (BR-014).
- Menambah lock aplikasi di jalur booking (BR-022).
- Mendaftarkan endpoint jalur gateway di fase 1. Ketiganya nonaktif (`../docs/04-api-spec.md` §3.8.1).
- Menandai invoice lunas dari redirect gateway. Hanya webhook terverifikasi (BR-064) — dan di
  fase 1 tidak ada halaman gateway sama sekali, jadi tidak ada redirect untuk dipercaya.
- Memproses webhook tanpa verifikasi tanda tangan, atau tanpa menyimpan payload mentah (BR-063).
- Membuat pembayaran kedua yang berhasil untuk satu invoice (BR-060).
- Menyimpan deposit negatif. Selisihnya jadi invoice baru (BR-048).
- Menambah baris `damage` tanpa `handover_photo_id` (BR-047).
- Mengirim pengingat telat lebih dari sekali sehari atau lebih dari 3 kali (BR-071).
- Menulis log berisi kredensial, token, atau nomor identitas. Daftar redaksi bersifat
  **allow-list** — field baru teredaksi secara default.
- Memasangkan nomor WhatsApp pribadi pemilik lewat Perangkat Tertaut. Cloud API resmi saja (BR-073).
- Mengembalikan `200` dengan body error.
- Membuat `trace_id` sendiri di luar konteks OpenTelemetry (BR-092).
- `time.Ticker` atau goroutine berjadwal di dalam `cmd/api` (BR-091).
- Menerima `slug` atau `owner_id` sebagai parameter di endpoint publik mana pun. Owner datang
  dari `Host`; parameter bisa dipalsukan siapa saja.
- **Membuka port aplikasi ke publik.** Ini bukan urusan ops yang terpisah — seluruh isolasi jalur
  publik bergantung padanya. Kalau aplikasi bisa dijangkau tanpa lewat proxy,
  `curl -H "Host: rentalbudi.sewain.id"` memilih pemilik mana pun yang diinginkan penyerang, dan
  isolasinya runtuh total, bukan bocor sedikit (BR-030).
- **Membaca `Host` sebagai sumber `owner_id` di `api.sewain.id`, atau menerima `X-API-Key` di
  `<slug>.sewain.id`.** Dua jalur, nol penyeberangan. Kunci sah + `Host` palsu = menunjuk pemilik
  lain, dan asumsi topologi yang menopang BR-030 tidak berlaku untuk host yang memang menerima
  panggilan dari luar browser (BR-032).
- **Memakai API key untuk apa pun di luar empat endpoint `04-api-spec.md` §4.** Ia bukan token
  backoffice dan bukan jalan ke data penyewa (BR-031).
- **Menerbitkan baris tagihan tanpa konfirmasi operator** — denda telat, kerusakan, dan potongan
  deposit semuanya diusulkan, bukan ditagih sendiri (BR-051).
- **Memperlakukan `0` dan `NULL` sebagai hal yang sama** pada `deposit_amount`, `late_fee_per_unit`,
  `min_duration`, `max_duration`. `NULL` = aturannya tidak berlaku; `0` ditolak database (BR-016).
- **Menyusun keadaan kalender di klien.** `state` dihitung server, satu definisi untuk semua layar
  (BR-033).

---

## Penjaga ruang lingkup fase 1

Tidak ada: tagihan bulanan berulang (itu fase 2 — kos), multi-cabang, kalender per jam, jadwal
berulang mingguan, GPS, integrasi asuransi, dynamic pricing, sinkronisasi offline, penghitungan
kapasitas unit fungible.

**API eksternal + CORS sekarang ADA di fase 1** (`S1-079`–`S1-081`, M5) — larangan lama soal
"jangan tulis satu baris pun kode CORS" sudah dicabut. Yang tetap dilarang adalah memakai kuncinya
di luar empat endpoint §4.

Kalau sebuah task menyiratkan salah satunya, itu milik fase berikutnya — berhenti dan katakan,
jangan bangun versi separuh yang nanti harus dibongkar. `../docs/05-backlog.md` §Di luar ruang lingkup punya
tabelnya.

**Langganan SaaS ditunda dari fase 1** (BR-080–BR-082). `subscriptions` tetap dibuat dan tetap
kosong, dan `invoices.kind`/`subscription_id` tetap jalan — jadi menyalakannya nanti nol migrasi.
Yang **tidak** dibangun: paket, penegakan kuota, tagihan langganan, `GET /subscription`. Fase 1
tanpa batas unit maupun pengguna, dan `unit-quota-exceeded` tidak pernah terbit.
