# TenantKu Backend

Backend TenantKu menyediakan API HTTP dengan Go, Gin, dan PostgreSQL. Layanan saat ini mencakup akun, profil usaha dan organisasi, draft acara, aturan harga, pembacaan booking, serta kontrak OpenAPI yang dapat dibuka melalui Scalar.

## Prasyarat

- Go 1.26.10 sesuai `go.mod`, `make`, dan `openssl`.
- PostgreSQL untuk development. Contoh setup di bawah menggunakan PostgreSQL 17.
- Frontend TenantKu untuk mencoba alur akun dari browser. Ikuti [README Frontend](https://github.com/M-Aidil-Fitrah/mbee_fe/blob/aidil/README.md).

## Setup lokal

1. Jika baru mengambil repositori, clone branch `aidil` dari direktori kerja.

   ```bash
   git clone --branch aidil git@github.com:athazayyan/mbee_mbeeee.git tenantku-backend
   cd tenantku-backend
   cp .env.example .env
   ```

2. Siapkan database pengembangan dan pengguna PostgreSQL yang memiliki izin menjalankan migration. Isi `DATABASE_URL` di `.env` sesuai koneksi tersebut. Jika belum punya PostgreSQL lokal dan tersedia Docker, contoh ini sesuai dengan nilai bawaan `.env.example`.

   ```bash
   docker run --name tenantku-postgres \
     -e POSTGRES_USER=tenantku \
     -e POSTGRES_PASSWORD=tenantku \
     -e POSTGRES_DB=tenantku \
     -p 127.0.0.1:5432:5432 \
     -v tenantku-postgres-data:/var/lib/postgresql/data \
     -d postgres:17
   ```

   Setelah container dibuat, gunakan `docker start tenantku-postgres` untuk menyalakannya kembali.

3. Jalankan perintah ini dua kali. Salin hasil pertama ke `JWT_SIGNING_KEY` dan hasil kedua ke `REFRESH_TOKEN_PEPPER` di `.env`.

   ```bash
   openssl rand -hex 32
   ```

   Kedua secret harus berbeda dan panjang masing-masing minimal 32 byte. `APP_ORIGIN` harus cocok dengan URL aplikasi FE di browser, yaitu `http://localhost:3001` pada setup bawaan. Tulis origin tanpa path atau garis miring penutup.

4. Setelah PostgreSQL siap, terapkan migration yang belum tercatat, kemudian nyalakan API.

   ```bash
   make migrate && go run .
   ```

   `make migrate` dan `go run .` membaca `.env` dari root repo. Variabel yang sudah ada di environment memiliki prioritas. Jalankan kembali `make migrate` setelah mengambil migration baru. Runner melewati versi yang sudah tercatat dan mengenali database yang sudah menjalankan migration akun awal.

## Worker kedaluwarsa booking

Setelah migration diterapkan, jalankan worker di terminal lain dari root repo.

```bash
go run . worker
```

Worker membaca `DATABASE_URL` dan memeriksa hold yang jatuh tempo setiap 5 detik. Untuk satu siklus pemrosesan, gunakan `go run . worker --once`. Pada deployment, jalankan `./tenantku worker` sebagai proses terpisah dari API. Log JSON worker memuat `job_id` saat booking dilepas atau terjadi kegagalan.

Untuk memantau proses kontinu, set `WORKER_HEALTH_ADDR=127.0.0.1:8081` atau alamat internal yang dapat dijangkau pemantau. Worker menjawab `GET /readyz` dengan 200 setelah satu siklus expiry berhasil, termasuk saat antrean kosong. Jawaban menjadi 503 jika siklus terakhir gagal atau 25 detik berlalu sejak keberhasilan terakhir. `worker --once` memakai kode keluar proses sebagai hasil.

## Hak akses PostgreSQL saat deployment

Siapkan tiga akun di setiap database TenantKu. Akun migration memiliki database dan tabel. Akun API dan akun worker adalah akun login khusus layanan dengan hak akses dari `deploy/postgres/runtime.sql`. Simpan kredensial tiap akun di konfigurasi secret environment masing-masing.

```bash
go build -o tenantku .
DATABASE_URL="$MIGRATION_DATABASE_URL" ./tenantku migrate
PGSERVICE=tenantku-migration psql -X -v api_role=tenantku_api -v worker_role=tenantku_worker \
  -f deploy/postgres/runtime.sql
```

Profil `tenantku-migration` menggunakan koneksi pemilik database. Ganti nilai `api_role` dan `worker_role` sesuai akun yang sudah dibuat. Jalankan skrip hanya pada database khusus TenantKu. Skrip menutup akses bawaan PostgreSQL untuk `PUBLIC` pada database tersebut dan menerapkan ulang hak akses kedua akun layanan setelah migration atau restore.

Set `DATABASE_URL` pada proses API ke akun API dan pada proses worker ke akun worker. Berikan `JWT_SIGNING_KEY`, `REFRESH_TOKEN_PEPPER`, dan `APP_ORIGIN` pada proses API. Set `API_DOCS_ENABLED=false` untuk API publik.

## Mengaktifkan quote dan hold

Setelah migration, pasang kebijakan booking yang disetujui dengan role pemilik database.

```bash
DATABASE_URL="$MIGRATION_DATABASE_URL" go run . hold-policy /path/to/policy.json
```

Pada deployment, ganti `go run .` dengan `./tenantku`. File JSON berisi `version`, `effective_at` dalam format RFC 3339 dengan zona waktu, `platform_fee` dan `provider_fee` dalam rupiah bulat, `refund_terms` untuk tenant, serta objek `refund_policy` yang disimpan di snapshot booking. Isi kedua fee secara eksplisit, termasuk jika disepakati bernilai nol. Menjalankan perintah ulang dengan isi yang sama aman. Perubahan kebijakan memakai versi dan waktu efektif baru.

Jalankan skrip role dengan `-v booking_hold=true`. Set `BOOKING_MAX_ACTIVE_HOLDS=1` untuk mengizinkan satu hold aktif per akun dalam satu acara. Durasi bawaan adalah 30 menit. Isi `BOOKING_HOLD_DURATION` jika perlu mengatur durasi untuk pengujian. Hold berakhir saat durasi habis atau slot berakhir, mana yang lebih dulu. Perubahan durasi berlaku untuk hold baru. API membaca versi kebijakan terbaru yang sudah berlaku dan mengembalikan 503 bila belum ada. Untuk menonaktifkan quote dan hold, kosongkan batas dan jalankan skrip role dengan nilai bawaan `false`.

## Pembayaran sandbox

Login menolak akun yang emailnya belum diverifikasi dengan `403 EMAIL_NOT_VERIFIED`, tetapi hanya setelah kata sandi cocok. Ini aktif secara bawaan. Isi `AUTH_REQUIRE_VERIFIED_EMAIL=false` untuk QA lokal. Akun lama yang belum terverifikasi tidak diubah otomatis. Di database QA, verifikasi manual dengan `UPDATE users SET email_verified_at = now() WHERE email_verified_at IS NULL;` atau minta tautan lewat halaman verifikasi email.

Migration mengisi tarif paket Mini hingga Large dan tarif dasar Custom. Custom menambah Rp5.000 per stand di atas 100. Pembelian lama tetap memakai harga dan kapasitas yang tercatat saat dibuat. Isi `EVENT_PLAN_REQUIRED=true` untuk mewajibkan paket aktif dan menegakkan kapasitas saat menyimpan denah dan menerbitkan acara. Pembelian paket berbayar memakai Midtrans Sandbox dan webhook yang sama dengan sewa stand.

Metode bayar sewa (E08) dirilis lewat tabel `payment_methods`: `bca_va` (Rp4.000) dan `other_qris` (Rp0), dengan biaya admin per metode yang ditanggung tenant. Total pembayaran adalah sewa ditambah biaya admin. Tarif bawaan adalah estimasi dan harus diganti dengan tarif kontrak. `go run . payments check` memverifikasi channel di sandbox dan `go run . payments reconcile` merekonsiliasi attempt dan ledger. Detail, ledger, dan langkah uji webhook hosted ada di [docs/pembayaran-midtrans.md](docs/pembayaran-midtrans.md). Isi `MIDTRANS_CLIENT_KEY` untuk perintah `check`.

Check-in, refund pembatalan 50%, sengketa, dan pencairan dana (E09) ada di paket `internal/funds`. Jalankan `go run . payments process` secara berkala untuk mengirim refund dan menutup transaksi provider milik booking yang dibatalkan. Pencairan dan refund manual dikonfirmasi admin di `/admin/dana`. Kebijakan, ledger, dan runbook ada di [docs/dana-refund-pencairan.md](docs/dana-refund-pencairan.md).

Untuk menguji API pembayaran, terapkan migration dan jalankan skrip role dengan `-v payment_sandbox=true`. Isi `MIDTRANS_ENV=sandbox` dan `MIDTRANS_SERVER_KEY` dengan kredensial sandbox di environment API. Kosongkan server key dan jalankan ulang skrip role tanpa flag sandbox saat pembayaran tidak digunakan. API memverifikasi status langsung ke Midtrans sebelum mengubah booking.

## Verifikasi lokal

| Endpoint | Hasil yang diharapkan |
| --- | --- |
| [GET /healthz](http://127.0.0.1:8080/healthz) | HTTP 200 dengan `{"status":"ok"}` |
| [GET /readyz](http://127.0.0.1:8080/readyz) | HTTP 200 dengan `{"status":"ready"}` jika PostgreSQL dan tabel aplikasi siap |
| [GET /api-reference](http://127.0.0.1:8080/api-reference) | Scalar saat `API_DOCS_ENABLED=true` |
| [GET /api/openapi.yaml](http://127.0.0.1:8080/api/openapi.yaml) | Dokumen OpenAPI saat `API_DOCS_ENABLED=true` |

Untuk membuka Scalar sebelum database siap, jalankan `DATABASE_URL= API_DOCS_ENABLED=true go run .`. Dalam mode ini, readiness dan endpoint akun mengembalikan HTTP 503. Pada deployment publik, biarkan flag dokumentasi kosong atau set `API_DOCS_ENABLED=false`.

Jika startup menampilkan `auth migration has not been applied`, periksa bahwa migration sudah diterapkan pada database dan schema yang dipakai `DATABASE_URL`. Jika endpoint akun mengembalikan HTTP 403 saat dipanggil dari browser, cocokkan `APP_ORIGIN` dengan origin FE.

## Pemeriksaan kode

```bash
make check
```

Perintah ini memeriksa format, menjalankan `go vet`, tes, dan build. Untuk tes integrasi sesi dan booking, siapkan database uji terpisah, isi `TENANTKU_TEST_DATABASE_URL`, dan terapkan migration sekali pada database uji sebelum menjalankan check. Tes booking membuat schema sementara di database uji.

```bash
export TENANTKU_TEST_DATABASE_URL='postgres://user:password@localhost:5432/tenantku_test?sslmode=disable'
DATABASE_URL="$TENANTKU_TEST_DATABASE_URL" make migrate
make check
```
