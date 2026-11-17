# Panel admin dan kalkulator (E16, E12)

## Izin admin (E16-01)

Akun admin tercatat di `platform_admins` dan memegang daftar izin. Server memeriksa izin di dalam transaksi yang melakukan aksinya.

| Izin | Mencakup |
| --- | --- |
| `directory.read` | ringkasan, pengguna, penyelenggara, acara |
| `organizations.verify` | verifikasi penyelenggara |
| `audit.read` | log audit |
| `payments.read` | pencarian booking dan pembayaran, antrean pengecualian |
| `payments.manage` | menutup pengecualian, memeriksa ulang pembayaran ke provider |
| `funds.manage` | refund sengketa, pencairan, sengketa, koreksi ledger |
| `pricing.manage` | tarif metode pembayaran dan versi harga paket |
| `moderation.manage` | moderasi pesan dan ulasan |

```sh
DATABASE_URL="$MIGRATION_DATABASE_URL" go run . grant-admin email@contoh.com                 # semua izin
DATABASE_URL="$MIGRATION_DATABASE_URL" go run . grant-admin email@contoh.com audit.read,payments.read   # dipersempit
DATABASE_URL="$MIGRATION_DATABASE_URL" go run . revoke-admin email@contoh.com
```

`GET /me` mengembalikan `admin_permissions`; sidebar panel hanya menampilkan menu yang diizinkan.

Log audit (`admin_audit`) hanya bisa ditambah: aktor, aksi, resource, alasan, sebelum dan sesudah, ID request, dan waktu. Nilai rahasia
(kata sandi, token, nomor rekening, kunci API) disamarkan sebelum disimpan. `GET /admin/audit` bisa difilter dengan `action`, `resource_type`, dan `resource_id`.

## Panel (E16-02)

| Halaman | Isi |
| --- | --- |
| `/admin/acara` | semua acara, dicari dengan ID, nama, penyelenggara, kota |
| `/admin/pembayaran` | booking, pembayaran, dan antrean pengecualian; menutup pengecualian dan memeriksa ulang pembayaran |
| `/admin/dana` | refund sengketa, pencairan, sengketa |
| `/admin/audit` | log audit |

Memeriksa ulang pembayaran meminta status asli ke Midtrans dan menerapkannya lewat jalur webhook yang idempoten, jadi aman diulang. Menutup
pengecualian yang sudah ditutup tidak mengubah apa pun dan tidak menulis audit kedua.

## Tarif dan harga paket (E16-03)

`/admin/tarif` mengelola:

- **Biaya admin per metode pembayaran** (`PUT /admin/payment-methods/{code}`). Perubahan hanya berlaku untuk pembayaran yang dibuat sesudahnya:
  tiap pembayaran menyimpan biaya dan `fee_version` saat dibuat. Mengubah nominal biaya wajib disertai `fee_version` baru.
- **Versi harga paket** (`POST /admin/plan-prices`). Versi baru wajib berlaku di masa depan dan tidak pernah mengubah versi lama, jadi pembelian yang sudah terjadi
  tetap memakai harganya. Daftar yang sama pada waktu yang sama dijawab 200 dengan versi yang ada; daftar berbeda pada waktu yang sama 409.
- Durasi hold, kebijakan refund, dan jadwal pencairan mengikuti kebijakan rilis dan belum bisa diubah dari panel (`/admin/settings/{key}` masih rencana).

Koreksi saldo memakai pembalikan ledger (`POST /admin/ledger/{id}/reverse`, izin `funds.manage`): transaksi baru dengan setiap entri dibalik, sekali per transaksi.
Belum ada layar untuk ini; ID transaksi ledger diambil dari database.

## Kalkulator titik impas (E12-01/02)

`POST /calculators/break-even` publik (120 permintaan per menit per alamat), tanpa akun, tanpa penyimpanan. Halaman: `/kalkulator` di aplikasi.

```
margin            = harga jual - biaya per unit
unit impas        = ceil(biaya tetap / margin)
omzet impas       = unit impas x harga jual
target harian     = ceil(unit impas / hari berjualan)
estimasi laba     = unit terjual x margin - biaya tetap   (negatif = rugi)
```

Bila margin nol atau negatif, `status` adalah `unreachable` dan ketiga target bernilai `null`. Batas input menjaga semua hasil tetap bilangan bulat yang
tepat di JSON: biaya tetap sampai Rp1 miliar, harga dan biaya per unit sampai Rp5 juta, penjualan sampai 10 juta unit, sampai 3.650 hari.
