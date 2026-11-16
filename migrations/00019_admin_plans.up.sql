-- E16 admin minimum dan katalog paket acara.
-- Admin platform hanya diberikan lewat perintah server (grant-admin), tidak pernah lewat registrasi.
CREATE TABLE platform_admins (
    user_id uuid PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    granted_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    note text CHECK (note IS NULL OR char_length(note) <= 200)
);

-- Jejak aksi admin. Tidak ada UPDATE/DELETE untuk role API, jadi audit hanya bisa ditambah.
CREATE TABLE admin_audit (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    actor_id uuid NOT NULL REFERENCES users (id),
    action text NOT NULL CHECK (char_length(action) BETWEEN 1 AND 80),
    resource_type text NOT NULL CHECK (char_length(resource_type) BETWEEN 1 AND 40),
    resource_id uuid,
    reason text NOT NULL CHECK (char_length(btrim(reason)) BETWEEN 1 AND 500),
    before jsonb,
    after jsonb,
    request_id text NOT NULL,
    occurred_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX admin_audit_time_idx ON admin_audit (occurred_at DESC, id DESC);
CREATE INDEX admin_audit_actor_idx ON admin_audit (actor_id, occurred_at DESC);

ALTER TABLE organizations ADD COLUMN verified_at timestamptz;

-- Paket per acara (PRD 9.1). Harga berversi dengan effective_at agar perubahan berlaku prospektif.
CREATE TABLE event_plan_versions (
    id integer GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    effective_at timestamptz NOT NULL UNIQUE,
    -- Paket Custom: harga Large ditambah tarif ini per tenant di atas kapasitas Large.
    custom_price_per_tenant bigint NOT NULL CHECK (custom_price_per_tenant BETWEEN 0 AND 9007199254740991),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE event_plans (
    version_id integer NOT NULL REFERENCES event_plan_versions (id) ON DELETE CASCADE,
    code text NOT NULL CHECK (code ~ '^[a-z]{2,20}$'),
    name text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 40),
    capacity integer NOT NULL CHECK (capacity BETWEEN 1 AND 100000),
    price bigint NOT NULL CHECK (price BETWEEN 0 AND 9007199254740991),
    PRIMARY KEY (version_id, code),
    UNIQUE (version_id, capacity)
);

WITH version AS (
    INSERT INTO event_plan_versions (effective_at, custom_price_per_tenant)
    VALUES ('2026-01-01T00:00:00+07:00', 5000) RETURNING id
)
INSERT INTO event_plans (version_id, code, name, capacity, price)
SELECT version.id, plan.code, plan.name, plan.capacity, plan.price FROM version, (VALUES
    ('mini', 'Mini', 20, 180000),
    ('basic', 'Basic', 30, 250000),
    ('standard', 'Standard', 40, 300000),
    ('plus', 'Plus', 50, 350000),
    ('medium', 'Medium', 75, 475000),
    ('large', 'Large', 100, 600000)
) AS plan (code, name, capacity, price);
