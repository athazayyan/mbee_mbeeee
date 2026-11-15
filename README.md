# TenantKu Backend

Backend TenantKu menyediakan API HTTP dengan Go, Gin, dan PostgreSQL. Layanan saat ini mencakup akun dan sesi, health check, serta kontrak OpenAPI yang dapat dibuka melalui Scalar.

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

## Verifikasi lokal

| Endpoint | Hasil yang diharapkan |
| --- | --- |
| [GET /healthz](http://127.0.0.1:8080/healthz) | HTTP 200 dengan `{"status":"ok"}` |
| [GET /readyz](http://127.0.0.1:8080/readyz) | HTTP 200 dengan `{"status":"ready"}` jika PostgreSQL dan tabel auth siap |
| [GET /api-reference](http://127.0.0.1:8080/api-reference) | Scalar saat `API_DOCS_ENABLED=true` |
| [GET /api/openapi.yaml](http://127.0.0.1:8080/api/openapi.yaml) | Dokumen OpenAPI saat `API_DOCS_ENABLED=true` |

Untuk membuka Scalar sebelum database siap, jalankan `DATABASE_URL= API_DOCS_ENABLED=true go run .`. Dalam mode ini, readiness dan endpoint akun mengembalikan HTTP 503. Pada deployment publik, biarkan flag dokumentasi kosong atau set `API_DOCS_ENABLED=false`.

Jika startup menampilkan `auth migration has not been applied`, periksa bahwa migration sudah diterapkan pada database dan schema yang dipakai `DATABASE_URL`. Jika endpoint akun mengembalikan HTTP 403 saat dipanggil dari browser, cocokkan `APP_ORIGIN` dengan origin FE.

## Pemeriksaan kode

```bash
make check
```

Perintah ini memeriksa format, menjalankan `go vet`, tes, dan build. Untuk tes integrasi sesi, siapkan database uji terpisah, isi `TENANTKU_TEST_DATABASE_URL`, dan terapkan migration sekali pada database uji sebelum menjalankan check.

```bash
export TENANTKU_TEST_DATABASE_URL='postgres://user:password@localhost:5432/tenantku_test?sslmode=disable'
DATABASE_URL="$TENANTKU_TEST_DATABASE_URL" make migrate
make check
```
