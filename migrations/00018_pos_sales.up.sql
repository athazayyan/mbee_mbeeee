-- E10 kasir: sesi per perangkat, penjualan yang disinkronkan dari kasir (bisa offline), dan rinciannya.
CREATE TABLE pos_sessions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id uuid NOT NULL REFERENCES businesses (id) ON DELETE CASCADE,
    device_id uuid NOT NULL,
    event_id uuid REFERENCES events (id),
    opened_by uuid NOT NULL REFERENCES users (id),
    opened_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    closed_at timestamptz,
    UNIQUE (business_id, id)
);

-- Satu perangkat hanya punya satu sesi terbuka per usaha.
CREATE UNIQUE INDEX pos_sessions_open_device_idx ON pos_sessions (business_id, device_id) WHERE closed_at IS NULL;

CREATE TABLE sales (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id uuid NOT NULL REFERENCES businesses (id) ON DELETE CASCADE,
    -- sale_id dibuat perangkat; dipakai untuk dedupe saat sync ulang.
    sale_id uuid NOT NULL,
    device_id uuid NOT NULL,
    session_id uuid NOT NULL,
    event_id uuid REFERENCES events (id),
    client_created_at timestamptz NOT NULL,
    received_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    schema_version integer NOT NULL CHECK (schema_version >= 1),
    payload_hash text NOT NULL CHECK (char_length(payload_hash) BETWEEN 1 AND 128),
    -- Digest kanonik buatan server untuk membedakan sync ulang yang identik dari payload yang berubah.
    request_digest bytea NOT NULL,
    currency text NOT NULL DEFAULT 'IDR' CHECK (currency = 'IDR'),
    method text NOT NULL CHECK (method IN ('cash', 'external')),
    discount bigint NOT NULL DEFAULT 0 CHECK (discount BETWEEN 0 AND 9007199254740991),
    total bigint NOT NULL CHECK (total BETWEEN 0 AND 9007199254740991),
    synced_by uuid NOT NULL REFERENCES users (id),
    UNIQUE (business_id, sale_id),
    UNIQUE (business_id, id),
    FOREIGN KEY (business_id, session_id) REFERENCES pos_sessions (business_id, id)
);

CREATE INDEX sales_business_time_idx ON sales (business_id, client_created_at DESC, id DESC);
CREATE INDEX sales_event_idx ON sales (event_id) WHERE event_id IS NOT NULL;

CREATE TABLE sale_items (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id uuid NOT NULL,
    sale_ref uuid NOT NULL,
    position smallint NOT NULL CHECK (position >= 0),
    product_id uuid NOT NULL,
    -- Snapshot saat transaksi; perubahan produk tidak mengubah riwayat.
    product_name text NOT NULL CHECK (char_length(product_name) BETWEEN 1 AND 120),
    quantity integer NOT NULL CHECK (quantity BETWEEN 1 AND 10000),
    unit_price bigint NOT NULL CHECK (unit_price BETWEEN 0 AND 9007199254740991),
    unit_cost bigint CHECK (unit_cost IS NULL OR unit_cost BETWEEN 0 AND 9007199254740991),
    discount bigint NOT NULL DEFAULT 0 CHECK (discount BETWEEN 0 AND 9007199254740991),
    subtotal bigint NOT NULL CHECK (subtotal BETWEEN 0 AND 9007199254740991),
    UNIQUE (sale_ref, position),
    FOREIGN KEY (business_id, sale_ref) REFERENCES sales (business_id, id) ON DELETE CASCADE,
    FOREIGN KEY (business_id, product_id) REFERENCES products (business_id, id)
);

ALTER TABLE inventory_movements ADD COLUMN sale_ref uuid REFERENCES sales (id) ON DELETE CASCADE;
CREATE INDEX inventory_movements_sale_idx ON inventory_movements (sale_ref) WHERE sale_ref IS NOT NULL;
