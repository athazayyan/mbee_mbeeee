-- Skema izin dan audit E16. Izin admin diberikan oleh operator database.
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
