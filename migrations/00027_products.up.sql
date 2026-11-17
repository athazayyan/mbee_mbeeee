-- E10 produk usaha dan mutasi stok. Stok di products adalah ringkasan dari inventory_movements.
CREATE TABLE products (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id uuid NOT NULL REFERENCES businesses (id) ON DELETE CASCADE,
    name text NOT NULL CHECK (char_length(btrim(name)) BETWEEN 1 AND 120),
    sku text CHECK (sku IS NULL OR char_length(sku) BETWEEN 1 AND 40),
    price bigint NOT NULL CHECK (price BETWEEN 0 AND 9007199254740991),
    cost bigint CHECK (cost IS NULL OR cost BETWEEN 0 AND 9007199254740991),
    category_id uuid REFERENCES business_categories (id),
    -- Kasir offline (E11) boleh membuat stok negatif; selisihnya ditandai untuk rekonsiliasi.
    stock integer NOT NULL DEFAULT 0,
    archived_at timestamptz,
    created_by uuid NOT NULL REFERENCES users (id),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (business_id, id)
);

CREATE INDEX products_business_idx ON products (business_id, created_at, id);
CREATE UNIQUE INDEX products_business_sku_idx ON products (business_id, upper(sku)) WHERE sku IS NOT NULL;

CREATE TABLE inventory_movements (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    business_id uuid NOT NULL,
    product_id uuid NOT NULL,
    delta integer NOT NULL CHECK (delta <> 0),
    reason text NOT NULL CHECK (reason IN ('initial', 'restock', 'correction', 'sale', 'return', 'void')),
    note text CHECK (note IS NULL OR char_length(note) <= 200),
    actor_id uuid REFERENCES users (id),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (business_id, product_id) REFERENCES products (business_id, id) ON DELETE CASCADE
);

CREATE INDEX inventory_movements_product_idx ON inventory_movements (product_id, id DESC);
