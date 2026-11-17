# Check-in, refund, sengketa, dan pencairan (E09)

Paket `internal/funds`. Kebijakan mengikuti PROD-03: refund 50% dari sewa saat penyelenggara membatalkan, biaya admin
pembayaran tetap menjadi biaya transaksi, dan pencairan atas pengajuan mulai H+4 hari kerja.

## Pembatalan acara (E09-03, E09-05)

`POST /events/{id}/cancel` memanggil `funds.SettleCancellation` di dalam transaksi yang sama dengan perubahan status acara.
Jika satu langkah gagal, acara tetap tidak berubah.

| Booking saat acara dibatalkan | Yang terjadi |
| --- | --- |
| `held` (hold atau penawaran kurasi) | dibatalkan, stand dilepas |
| `payment_pending` | dibatalkan, stand dilepas, attempt pembayaran ditutup, transaksi di provider ditutup oleh `payments process` |
| `confirmed` | dibatalkan, stand dilepas, refund 50% dari sewa snapshot (`refunds`), dana disisihkan dari escrow |
| pengajuan menunggu keputusan | ditarik (`withdrawn`) |

- Tiap booking yang ditutup mendapat riwayat dengan alasan `event_cancelled`. Worker notifikasi (E15) mengubahnya menjadi
  pesan untuk tenant.
- Contoh (E09-06): sewa Rp100.000 dan admin Rp2.000 menghasilkan refund Rp50.000. Admin Rp2.000 tidak dikembalikan.
- Pembayaran yang masih masuk sesudah pembatalan menjadi exception `late_payment` lewat webhook, karena booking sudah
  dibatalkan. Operator mengembalikannya manual.
- `GET /events/{id}/cancellation-preview` menampilkan dampak sebelum penyelenggara mengonfirmasi.

## Refund

Status: `requested` → `refunded`, atau `manual_required` / `failed`.

- `go run . payments process` mengirim refund yang `requested`. Metode dengan `refund_mode = 'api'` dikirim ke Midtrans dengan
  `refund_key` sama dengan ID refund, sehingga pengulangan tidak membayar dua kali. Balasan "tidak didukung" diperiksa lewat
  API status: bila transaksi sudah berstatus `refund` atau `partial_refund`, refund dianggap selesai.
- Metode `manual` (BCA VA dan QRIS saat ini) langsung menjadi `manual_required` dan masuk antrean exception
  `manual_refund_required`. Admin mentransfer ke tenant lalu menekan **Sudah dikembalikan** di `/admin/dana`
  dengan nomor referensi. Biaya pemrosesan refund (bila ada) diisi saat konfirmasi dan dibebankan ke escrow penyelenggara.
- Setelah lima kali gagal menghubungi provider, refund menjadi `manual_required`.
- Jalankan `payments process` tiap beberapa menit (cron), dengan `DATABASE_URL` setingkat API.

## Ledger

Dana yang sudah dialokasikan tetapi belum keluar dari provider berada di akun penampung, sehingga pencairan tidak bisa
memakainya dua kali.

| Peristiwa | Debit | Kredit |
| --- | --- | --- |
| refund diajukan (`refund_reserved`) | `organizer_escrow` | `refund_payable` |
| refund terkirim (`refund_issued`) | `refund_payable` | `provider_clearing` |
| biaya proses refund | `organizer_escrow` | `provider_fee_payable` |
| pencairan diajukan (`payout_reserved`) | `organizer_escrow` | `payout_payable` |
| pencairan terkirim (`payout_released`) | `payout_payable` | `provider_clearing` |
| pencairan gagal (`reversal`) | `payout_payable` | `organizer_escrow` |

## Pencairan dana (E09-04)

`POST /events/{id}/payouts` dengan `Idempotency-Key`. Syarat, semuanya diperiksa dalam satu transaksi:

1. Pemilik organisasi.
2. Hari ini di zona waktu acara ≥ hari terakhir acara + 4 hari kerja. Akhir pekan dilewati, dan libur nasional diisi di
   tabel `non_working_days` (`INSERT INTO non_working_days (day, note) VALUES ('2026-12-25', 'Natal')`). Acara yang
   dibatalkan memakai jadwal asalnya.
3. Rekening tersimpan (`PUT /organizations/{id}/bank-account`). Rekening disalin ke pengajuan dan tidak bisa diubah
   selama ada pencairan menunggu transfer.
4. Tidak ada refund yang belum selesai dan tidak ada sengketa terbuka untuk acara itu.
5. Dana tersedia lebih dari nol. Jumlahnya semua escrow acara dikurangi refund, biaya refund, dan pencairan sebelumnya.
6. Satu pencairan aktif atau selesai per acara. Kunci yang sama mengembalikan pencairan yang sama.

Status dipegang admin: pencairan menjadi `released` hanya setelah admin menekan **Sudah ditransfer** di `/admin/dana`
dengan referensi transfer. Pencairan gagal mengembalikan dana ke escrow dan boleh diajukan ulang.

**Belum ada disbursement otomatis.** Transfer ke rekening penyelenggara dilakukan operator. Mengotomatiskannya memerlukan
Midtrans Iris atau bank lain (PROD-02). Antarmukanya sudah terpisah: yang perlu ditambah hanya pemanggil yang memakai
`ReleasePayout` setelah provider mengonfirmasi.

## Sengketa

Tenant membuka sengketa pada booking yang sudah dibayar selama dana acara belum dicairkan
(`POST /bookings/{id}/disputes`). Satu sengketa terbuka per booking, dan sengketa terbuka menahan pencairan acara.
Admin memutuskan di `/admin/dana`: refund (jumlah sampai sisa sewa yang belum dikembalikan) atau tolak. Refund sengketa
mengikuti jalur refund di atas.

## Check-in (E09-02)

- Tenant meminta kode di `POST /bookings/{id}/check-in-code`: 10 huruf dan angka tanpa I dan O, tetap sama setelah dibuat.
  Teks QR adalah `tenantku:checkin:<kode>`. FE membuat QR sendiri tanpa dependensi.
- Staf (pemilik atau anggota organisasi) memanggil `POST /events/{id}/check-ins` dengan `code` atau `booking_id`.
  Kode boleh huruf besar atau kecil dan tanda hubung diabaikan.
- Booking harus terkonfirmasi untuk acara itu. Periode check-in: 2 jam sebelum acara mulai sampai acara berakhir.
- Idempoten: pemindaian ulang mengembalikan catatan pertama (HTTP 200, `already: true`), bahkan di luar periode.
- Kode dari acara lain tidak ditemukan, jadi tidak bisa dipakai untuk menebak.

## Menjalankan

```sh
make migrate                       # migrasi 00037
# terapkan ulang deploy/postgres/runtime.sql (-v payment_sandbox=true)
go run . payments process          # refund dan penutupan transaksi, jalankan berkala
```

## Yang masih terbuka

- Refund otomatis per metode (PROD-02): hanya metode dengan `refund_mode = 'api'` memakai API Midtrans. Perilaku
  sandbox untuk refund GoPay belum diuji ke Midtrans sungguhan.
- Disbursement otomatis ke penyelenggara (Iris).
- Notifikasi status refund dan pencairan. Pembatalan sudah memicu notifikasi lewat riwayat booking.
- Libur nasional perlu diisi ke `non_working_days` oleh operator.
