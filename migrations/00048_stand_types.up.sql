-- Jenis stand untuk denah bergambar: menentukan ilustrasi di editor dan di peta yang dilihat tenant.
-- Kosong berarti stand umum. Gerbang keluar ditambahkan sebagai jenis area di samping pintu masuk.
ALTER TABLE stands ADD COLUMN stand_type text CHECK (stand_type IS NULL OR stand_type IN (
    'makanan_berat', 'minuman', 'dessert', 'fresh_product', 'pakaian', 'aksesoris', 'beauty', 'photobooth', 'lainnya'));

ALTER TABLE layout_areas DROP CONSTRAINT IF EXISTS layout_areas_kind_check;
ALTER TABLE layout_areas ADD CONSTRAINT layout_areas_kind_check
    CHECK (kind IN ('walkway', 'entrance', 'exit', 'stage', 'restricted'));
