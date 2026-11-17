-- Unggahan file (E10 foto produk; dipakai juga banner acara). Klien meminta intent, mengunggah ke URL bertenggat,
-- lalu menyelesaikan unggahan. Hanya unggahan berstatus ready yang boleh dipakai resource lain.
CREATE TABLE uploads (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    purpose text NOT NULL CHECK (purpose IN ('product_photo', 'event_banner', 'dispute_evidence')),
    owner_type text NOT NULL CHECK (owner_type IN ('business', 'organization')),
    owner_id uuid NOT NULL,
    content_type text NOT NULL CHECK (content_type IN ('image/jpeg', 'image/png', 'image/webp')),
    size_bytes integer NOT NULL CHECK (size_bytes BETWEEN 1 AND 5242880),
    storage_key text NOT NULL UNIQUE CHECK (storage_key ~ '^[a-z0-9/_-]{1,200}$'),
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'ready', 'rejected')),
    reject_reason text CHECK (reject_reason IS NULL OR char_length(reject_reason) <= 120),
    created_by uuid NOT NULL REFERENCES users (id),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    expires_at timestamptz NOT NULL,
    completed_at timestamptz,
    CHECK ((status = 'pending') = (completed_at IS NULL))
);

CREATE INDEX uploads_creator_recent_idx ON uploads (created_by, created_at DESC);
CREATE INDEX uploads_pending_expiry_idx ON uploads (expires_at) WHERE status = 'pending';

ALTER TABLE products ADD COLUMN photo_upload_id uuid REFERENCES uploads (id);
