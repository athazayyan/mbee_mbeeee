# Akun dan data demo

Perintah `seed-demo` mengisi database pengembangan dengan satu admin, lima penyelenggara (masing-masing satu acara terbit di
Banda Aceh atau Lhokseumawe), dan 30 tenant lengkap dengan usaha, produk, stok, dan persetujuan data. Data ini fiktif dan hanya untuk
pengembangan dan demo.

```sh
make migrate
# terapkan ulang deploy/postgres/runtime.sql dengan -v booking_hold=true -v payment_sandbox=true
# isi BOOKING_MAX_ACTIVE_HOLDS=1 di .env agar tenant bisa menahan stand
DATABASE_URL="$MIGRATION_DATABASE_URL" go run . seed-demo
```

Gunakan akun migration (pemilik database), bukan akun API. Perintah aman diulang: akun yang sudah ada dipakai lagi dan kata
sandinya dikembalikan ke nilai di bawah, acara yang slug-nya sudah ada dilewati. Tanggal acara dihitung dari hari perintah dijalankan, jadi
selalu di masa depan.

## Aturan yang dipakai data ini

- **Satu akun, satu penyelenggara.** Akun hanya boleh punya satu profil penyelenggara (409 `ORGANIZATION_ALREADY_EXISTS`).
- **Satu penyelenggara, satu acara aktif.** Acara baru (atau duplikat) baru bisa dibuat setelah acara yang ada selesai atau dibatalkan
  (409 `ACTIVE_EVENT_EXISTS`).
- **Satu akun, satu usaha tenant.** Usaha kedua ditolak (409 `BUSINESS_ALREADY_EXISTS`).
- **Satu tenant, satu acara dalam satu waktu.** Selama hold, pembayaran, atau booking terkonfirmasi di sebuah acara belum selesai, hold dan
  pengajuan di acara lain ditolak (409 `ACTIVE_PARTICIPATION_EXISTS`). Setelah acara itu berakhir atau dibatalkan, tenant bisa ikut acara berikutnya.
- Penyelenggara mengisi akun Instagram acara pada langkah Informasi saat membuat draft. Akun itu tampil di halaman acara tenant dan situs publik.

## Jumlah tenant

Lima acara punya 81 stand, sedangkan seorang tenant hanya bisa ikut satu acara dalam satu waktu. Karena itu 30 tenant disiapkan: cukup untuk mengisi
sebagian besar stand satu per satu, dan 4 di antaranya sudah mengajukan ke acara berkurasi (Festival Kopi Ulee Kareng) sehingga antrean kurasi
penyelenggara langsung terisi. Untuk mengisi seluruh stand, buat tenant tambahan lewat halaman daftar.

## Kata sandi

| Peran | Kata sandi |
| --- | --- |
| Admin | `admin123456` |
| Semua penyelenggara | `Penyelenggara123` |
| Semua tenant | `Tenant123456` |

Daftar akun lengkap ada di bawah. Kolom ID acara berbeda tiap database, lihat di URL halaman acara.

## Akun admin
| Email | Password |
| --- | --- |
| admin@tenantku.com | admin123456 |

## Akun penyelenggara (satu akun, satu organisasi, satu acara aktif)
| Email | Password | Organisasi | Acara | Kota | Stand | Instagram |
| --- | --- | --- | --- | --- | --- | --- |
| eo.peunayong@tenantku.com | Penyelenggara123 | Komunitas Peunayong Kreatif | Pasar Kreatif Peunayong 2026 | Banda Aceh | 14 | peunayong.kreatif |
| eo.tamansari@tenantku.com | Penyelenggara123 | Taman Sari Event Organizer | Banda Aceh Night Market Taman Sari | Banda Aceh | 18 | nightmarket.bna |
| eo.acehcoffee@tenantku.com | Penyelenggara123 | Aceh Coffee Society | Festival Kopi Ulee Kareng | Banda Aceh | 14 | acehcoffeesociety |
| eo.umkmlsm@tenantku.com | Penyelenggara123 | Forum UMKM Lhokseumawe | Bazar UMKM Pantai Ujong Blang | Lhokseumawe | 15 | umkm.lhokseumawe |
| eo.paseeevent@tenantku.com | Penyelenggara123 | Pasee Event Kreatif | Lhokseumawe Culinary Night Fest | Lhokseumawe | 20 | lsm.culinaryfest |

## Akun tenant (satu akun, satu usaha, ikut satu acara dalam satu waktu)
| Email | Password | Usaha | Kategori | Catatan |
| --- | --- | --- | --- | --- |
| tenant01@tenantku.com | Tenant123456 | Mie Aceh Bang Ucok | Makanan dan minuman |  |
| tenant02@tenantku.com | Tenant123456 | Kopi Sanger Seulanga | Makanan dan minuman | sudah mengajukan ke festival-kopi-ulee-kareng |
| tenant03@tenantku.com | Tenant123456 | Kue Adee Cut Nyak | Makanan dan minuman |  |
| tenant04@tenantku.com | Tenant123456 | Timphan Bu Cut | Makanan dan minuman |  |
| tenant05@tenantku.com | Tenant123456 | Keumamah Dapoe Aceh | Makanan dan minuman |  |
| tenant06@tenantku.com | Tenant123456 | Sate Matang Pak Yusuf | Makanan dan minuman |  |
| tenant07@tenantku.com | Tenant123456 | Martabak Aceh Pasee | Makanan dan minuman |  |
| tenant08@tenantku.com | Tenant123456 | Es Timun Serut Lhok | Makanan dan minuman |  |
| tenant09@tenantku.com | Tenant123456 | Roti Cane Kari Gampong | Makanan dan minuman |  |
| tenant10@tenantku.com | Tenant123456 | Kopi Ulee Kareng Meuligoe | Makanan dan minuman | sudah mengajukan ke festival-kopi-ulee-kareng |
| tenant11@tenantku.com | Tenant123456 | Warkop Tikoe Lampulo | Makanan dan minuman | sudah mengajukan ke festival-kopi-ulee-kareng |
| tenant12@tenantku.com | Tenant123456 | Gayo Highland Bean | Makanan dan minuman | sudah mengajukan ke festival-kopi-ulee-kareng |
| tenant13@tenantku.com | Tenant123456 | Wajeb Kampoeng Lhokseumawe | Makanan dan minuman |  |
| tenant14@tenantku.com | Tenant123456 | Dapur Pasee Kuah Beulangong | Makanan dan minuman |  |
| tenant15@tenantku.com | Tenant123456 | Boba Lampineung | Makanan dan minuman |  |
| tenant16@tenantku.com | Tenant123456 | Songket Aceh Meuligoe | Fesyen |  |
| tenant17@tenantku.com | Tenant123456 | Batik Aceh Pucok Reudeuk | Fesyen |  |
| tenant18@tenantku.com | Tenant123456 | Kaos Haba Aceh | Fesyen |  |
| tenant19@tenantku.com | Tenant123456 | Hijab Lampuuk | Fesyen |  |
| tenant20@tenantku.com | Tenant123456 | Rencong Mini Kriya Aceh | Kerajinan |  |
| tenant21@tenantku.com | Tenant123456 | Anyaman Pandan Aceh Utara | Kerajinan |  |
| tenant22@tenantku.com | Tenant123456 | Tenun Pandan Blang Pidie | Kerajinan |  |
| tenant23@tenantku.com | Tenant123456 | Keramik Gampong Raya | Kerajinan |  |
| tenant24@tenantku.com | Tenant123456 | Foto Polaroid Ulee Lheue | Jasa |  |
| tenant25@tenantku.com | Tenant123456 | Henna dan Make Up Dara Baro | Jasa |  |
| tenant26@tenantku.com | Tenant123456 | Sablon Kaos Kilat Banda | Jasa |  |
| tenant27@tenantku.com | Tenant123456 | Souvenir Gayo Mini | Lainnya |  |
| tenant28@tenantku.com | Tenant123456 | Tanaman Hias Lamteumen | Lainnya |  |
| tenant29@tenantku.com | Tenant123456 | Madu Hutan Aceh Tengah | Lainnya |  |
| tenant30@tenantku.com | Tenant123456 | Buku dan Komik Lokal Aceh | Lainnya |  |

## Mencoba alurnya

1. **Admin**: masuk sebagai `admin@tenantku.com`, buka `/admin`. Menu Booking dan pembayaran, Tarif dan harga paket, Dana dan refund, dan Log audit ada di sidebar.
2. **Penyelenggara**: masuk sebagai salah satu akun EO, buka Acara. Acaranya sudah terbit dengan denah, harga per blok, dan syarat. Buka acara,
   lalu **Edit denah** untuk melihat atau mengubah denah (`/penyelenggara/acara/{id}/layout`). Acara Festival Kopi Ulee Kareng memakai mode kurasi:
   buka **Kurasi** untuk memutuskan pengajuan tenant.
3. **Tenant**: masuk sebagai `tenant01@tenantku.com`. Halaman awal **Cari acara** menampilkan acara yang dibuka. Pilih acara, klik petak stand
   di denah, lalu **Tahan stand** dan bayar. Setelah punya booking aktif, acara lain tidak bisa dipesan sampai acara itu selesai.
4. **Situs publik**: `http://localhost:3000/acara` memuat katalog dari API (`NUXT_API_ORIGIN`, bawaan `http://127.0.0.1:8080`), jadi API harus berjalan dan
   database sudah di-seed.
