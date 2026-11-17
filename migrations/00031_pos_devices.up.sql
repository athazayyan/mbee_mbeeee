-- E11 perangkat kasir. Perangkat terdaftar saat pertama membuka sesi; pemilik dapat mencabut dan memulihkannya.
-- Perangkat yang dicabut tidak bisa membuka sesi atau sync, tetapi transaksi lokalnya tidak dihapus.
CREATE TABLE pos_devices (
    business_id uuid NOT NULL REFERENCES businesses (id) ON DELETE CASCADE,
    id uuid NOT NULL,
    label text CHECK (label IS NULL OR char_length(label) BETWEEN 1 AND 80),
    registered_by uuid NOT NULL REFERENCES users (id),
    registered_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    last_seen_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    last_synced_at timestamptz,
    revoked_at timestamptz,
    revoked_by uuid REFERENCES users (id),
    revoke_reason text CHECK (revoke_reason IS NULL OR char_length(revoke_reason) BETWEEN 1 AND 200),
    PRIMARY KEY (business_id, id)
);

-- Perangkat dari sesi yang sudah ada sebelum tabel ini dibuat.
INSERT INTO pos_devices (business_id, id, registered_by, registered_at, last_seen_at)
SELECT DISTINCT ON (business_id, device_id) business_id, device_id, opened_by, opened_at, opened_at
FROM pos_sessions ORDER BY business_id, device_id, opened_at;
