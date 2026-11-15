# Panduan kontribusi

## Branch dan commit

- Kerjakan perubahan pada branch pribadi, misalnya `aidil`, dan sinkronkan dengan branch utama sesuai alur tim.
- Buat commit kecil untuk satu perubahan yang saling berkaitan. Sekitar tiga file per commit dapat menjadi acuan, bukan batas kaku; kelompokkan berdasarkan tujuan dan pastikan perubahan tetap utuh.
- Commit dan push setiap unit pekerjaan yang sudah siap dan telah diperiksa. Jangan menumpuk seluruh pekerjaan hingga akhir.
- Gunakan pesan singkat dan profesional dengan format `type(scope): description`, misalnya `docs(readme): update readme` atau `fix(auth): validate session`.
- Gunakan identitas Git masing-masing kontributor. Tambahkan `Co-authored-by` hanya untuk orang yang benar-benar ikut mengerjakan commit; jangan menambahkan atribusi agen AI.
- Sebelum commit, periksa `git status` dan `git diff --cached`, serta jalankan pemeriksaan yang relevan. Jangan commit secret atau file lokal.
- Sebelum push, periksa branch tujuan dan sinkronkan perubahan remote tanpa force push.
