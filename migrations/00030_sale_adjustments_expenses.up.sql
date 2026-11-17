-- E10-05 koreksi penjualan (void/retur) dan pengeluaran usaha. Semua koreksi append-only:
-- transaksi asal tidak pernah diubah, pembalikan dicatat sebagai baris baru.
CREATE TABLE sale_adjustments (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id uuid NOT NULL,
    sale_ref uuid NOT NULL,
    type text NOT NULL CHECK (type IN ('void', 'return')),
    reason text NOT NULL CHECK (char_length(btrim(reason)) BETWEEN 1 AND 300),
    amount bigint NOT NULL CHECK (amount BETWEEN 0 AND 9007199254740991),
    created_by uuid NOT NULL REFERENCES users (id),
    idempotency_key text NOT NULL CHECK (char_length(idempotency_key) BETWEEN 1 AND 128),
    request_digest bytea NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (business_id, created_by, idempotency_key),
    UNIQUE (business_id, id),
    FOREIGN KEY (business_id, sale_ref) REFERENCES sales (business_id, id) ON DELETE CASCADE
);

-- Satu transaksi hanya bisa di-void sekali.
CREATE UNIQUE INDEX sale_adjustments_one_void_idx ON sale_adjustments (sale_ref) WHERE type = 'void';
CREATE INDEX sale_adjustments_sale_idx ON sale_adjustments (sale_ref, created_at);

CREATE TABLE sale_adjustment_items (
    adjustment_id uuid NOT NULL REFERENCES sale_adjustments (id) ON DELETE CASCADE,
    sale_item_id uuid NOT NULL REFERENCES sale_items (id) ON DELETE CASCADE,
    quantity integer NOT NULL CHECK (quantity BETWEEN 1 AND 10000),
    PRIMARY KEY (adjustment_id, sale_item_id)
);

CREATE INDEX sale_adjustment_items_item_idx ON sale_adjustment_items (sale_item_id);

ALTER TABLE inventory_movements ADD COLUMN adjustment_ref uuid REFERENCES sale_adjustments (id) ON DELETE CASCADE;

CREATE TABLE expenses (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id uuid NOT NULL REFERENCES businesses (id) ON DELETE CASCADE,
    event_id uuid REFERENCES events (id),
    category text NOT NULL CHECK (category IN ('sewa', 'bahan', 'operasional', 'transportasi', 'tenaga', 'lainnya')),
    amount bigint NOT NULL CHECK (amount BETWEEN 1 AND 9007199254740991),
    occurred_at timestamptz NOT NULL,
    note text CHECK (note IS NULL OR char_length(note) BETWEEN 1 AND 200),
    created_by uuid NOT NULL REFERENCES users (id),
    idempotency_key text NOT NULL CHECK (char_length(idempotency_key) BETWEEN 1 AND 128),
    request_digest bytea NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (business_id, created_by, idempotency_key)
);

CREATE INDEX expenses_business_time_idx ON expenses (business_id, occurred_at DESC, id DESC);
