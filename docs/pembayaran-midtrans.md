# Pembayaran Midtrans (E08)

Dokumen ini mencatat keputusan dan cara mengoperasikan pembayaran sandbox. Tarif dan metode berada di tabel
`payment_methods`, jadi mengubahnya tidak butuh perubahan kode.

## Metode yang dirilis (E08-01)

| Kode | Metode | Channel Snap | `payment_type` di notifikasi | Biaya admin (estimasi) | Refund |
| --- | --- | --- | --- | --- | --- |
| `bank_transfer` | Virtual Account | `bca_va`, `bni_va`, `bri_va`, `permata_va`, `other_va` | `bank_transfer` | Rp4.000 per transaksi | manual |
| `qris` | QRIS | `other_qris` | `qris` | 0,7% | manual |
| `gopay` | GoPay | `gopay` | `gopay` | 2% | API |

- Kartu kredit, minimarket, dan ShopeePay tidak dirilis. Tambahkan lewat baris baru di `payment_methods` setelah
  channel-nya aktif dan `payments check` lulus.
- **Tarif masih estimasi** (`fee_source = 'estimate'`), diambil dari daftar harga publik Midtrans. Ganti dengan
  tarif kontrak merchant lalu set `fee_source = 'contract'`. Ini bagian dari PROD-02.
- **Yang menanggung biaya adalah tenant** (PROD-03). Biaya dihitung dari bawah ke atas supaya setelah Midtrans memotong
  biayanya, platform tetap menerima sewa + biaya platform: `bruto = ceil((dasar + flat) × 10000 / (10000 − bps))`.
  PPN atas biaya Midtrans belum diperhitungkan.
- **Refund**: kolom `refund_mode` menandai metode yang bisa di-refund lewat API (`api`) dan yang harus dikembalikan
  manual ke rekening tenant (`manual`). Nilainya perkiraan dan harus dikonfirmasi ke Midtrans sebelum E09 dikerjakan.
- **Pencairan ke penyelenggara** tidak bergantung pada metode tenant. Ia lewat disbursement terpisah (E09) dan belum ada.

## Alur checkout (E08-02, E08-05)

1. `GET /bookings/{id}/payment-options` menampilkan sewa, biaya platform, dan total tiap metode.
2. `POST /bookings/{id}/payments` dengan `{ "method": "qris" }` membuat attempt. Server menghitung total; klien tidak
   mengirim nominal. Halaman bayar Midtrans dibatasi ke channel metode itu.
3. Tenant dialihkan ke `checkout_url`. Setelah selesai Midtrans mengirim browser ke
   `{APP_ORIGIN}/tenant/bookings/{id}?pembayaran=selesai`. Halaman itu menunggu status dari server, bukan dari query.
4. Mengganti metode saat attempt masih aktif menutup transaksi lama di Midtrans (`expire`) dan membuat attempt baru
   dengan total baru. Bila Midtrans menolak karena transaksi lama sedang dibayar, server menjawab
   `409 PAYMENT_IN_PROGRESS`.
5. Metode yang benar-benar dipakai (`payment_type` dari Midtrans) disimpan di `payment_attempts.confirmed_type`. Bila
   berbeda dari metode yang dipilih, uangnya tetap dicatat dan muncul exception `method_mismatch`.

Biaya provider tetap pada snapshot booking (`bookings.provider_fee`) tidak lagi ditagihkan. Biaya admin metode
menggantikannya. Karena itu hold policy sebaiknya mengisi `provider_fee` dengan 0.

## Ledger (E08-04)

Satu pembayaran sewa menghasilkan satu transaksi `payment_received`:

| Akun | Arah | Isi |
| --- | --- | --- |
| `provider_clearing` | debit | total yang dibayar tenant |
| `organizer_escrow` | kredit | sewa, hak hasil penyelenggara |
| `platform_revenue` | kredit | biaya platform |
| `provider_fee_payable` | kredit | biaya admin metode |

Jenis transaksi lain: `plan_payment_received` (harga paket, seluruhnya pendapatan platform), `reversal` (cermin dari
transaksi asal), `refund_issued` dan `payout_released` (disediakan untuk E09). Ledger append-only dan setiap transaksi
harus seimbang saat commit.

Entri pembalik: `Store.ReversePayment(attemptID)`. Menyelesaikan exception: `Store.ResolveException(id)`.

## Rekonsiliasi (E08-04)

```sh
go run . payments reconcile
```

- Attempt yang masih terbuka lebih dari 2 menit dicek ke API status Midtrans dan diproses lewat jalur yang sama
  dengan webhook, sehingga webhook yang terlewat tersusul. Mengulang perintah ini aman.
- Attempt yang belum pernah dibuka di Midtrans dan sudah lewat 10 menit dari batas waktunya ditutup.
- Attempt `paid` tanpa transaksi ledger menjadi exception `ledger_missing`. Transaksi ledger yang debit
  `provider_clearing`-nya berbeda dari nominal attempt menjadi `ledger_mismatch`.
- Keluaran JSON memuat saldo tiap akun dan `balanced`. Perintah keluar dengan kode non-nol bila ledger tidak seimbang
  atau ada exception ledger, jadi bisa dipasang di cron atau CI. Jalankan tiap 5 menit selama sandbox aktif.

Perintah memakai `DATABASE_URL` dengan hak seperti API. Worker kedaluwarsa booking memakai role terbatas dan tidak
menjalankannya.

## Memeriksa metode di sandbox (E08-01)

```sh
go run . payments check
```

Perintah membuat token Snap sekali pakai per metode lalu membaca channel yang benar-benar ditawarkan Midtrans untuk
akun merchant. Perlu `MIDTRANS_CLIENT_KEY`. Midtrans menerima nama channel apa pun saat token dibuat, jadi pembacaan
ulang itulah yang membuktikan channel aktif. Tidak ada uang yang bergerak dan tidak ada data tersimpan.

Hasil pada akun sandbox proyek ini (17 November 2026): `bank_transfer`, `qris`, dan `gopay` tersedia.

## Uji webhook hosted (E08-03, E08-06)

Yang sudah terbukti terhadap sandbox Midtrans yang asli: pembuatan token dengan channel terbatas, pembacaan channel
aktif, status sebelum pembayar memilih metode (`404` di dalam respons HTTP 200), status VA `pending` dengan
`payment_type` dan bank, `expire` pada VA yang pending (`407`), dan `expire` ulang (`412`).

Yang masih harus dijalankan manual, karena butuh tautan publik dan simulator Midtrans:

1. Jalankan API lokal dan buka tunnel HTTPS, misalnya `cloudflared tunnel --url http://127.0.0.1:8080` atau `ngrok http 8080`.
2. Di dashboard sandbox Midtrans, Settings > Payment > **Payment Notification URL**, isi
   `https://<tunnel>/api/v1/webhooks/payments/midtrans`. Isi **Finish Redirect URL** dengan `{APP_ORIGIN}/tenant/bookings`.
3. Buat booking, pilih metode, lalu bayar di <https://simulator.sandbox.midtrans.com/>:
   - VA: masukkan nomor VA dari halaman bayar.
   - QRIS dan GoPay: pakai simulator QRIS atau GoPay di halaman yang sama.
4. Pastikan: booking menjadi `confirmed`, `payment_events` mencatat notifikasi dengan `outcome = 'applied'`,
   `payment_attempts.confirmed_type` terisi, ledger punya satu transaksi `payment_received`, dan
   `go run . payments reconcile` melaporkan `balanced: true`.
5. Skenario yang perlu diulang di hosted: bayar setelah hold lewat batas (harus menjadi exception `late_payment`),
   ganti metode lalu bayar lewat halaman lama, dan matikan API sebentar saat membayar lalu pastikan Midtrans
   mengirim ulang notifikasi.

Catat hasilnya di TASKS.md pada E08-03 dan E08-06 setelah dijalankan.

## Pertanyaan yang masih terbuka (PROD-02)

- Tarif kontrak per metode, dan apakah PPN dibebankan ke tenant.
- Metode yang boleh di-refund sebagian lewat API untuk refund 50% (E09).
- Mekanisme pencairan ke rekening penyelenggara (Midtrans Iris atau transfer manual).
- Apakah metode tambahan (kartu, minimarket) perlu dirilis untuk pilot.
