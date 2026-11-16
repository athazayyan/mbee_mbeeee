CREATE TABLE organization_event_history (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL REFERENCES organizations (id),
    reported_by uuid NOT NULL REFERENCES users (id),
    name text NOT NULL CHECK (char_length(btrim(name)) BETWEEN 1 AND 160),
    starts_at timestamptz NOT NULL,
    ends_at timestamptz NOT NULL,
    timezone text NOT NULL CHECK (char_length(timezone) BETWEEN 1 AND 64),
    city text NOT NULL CHECK (char_length(btrim(city)) BETWEEN 1 AND 120),
    address text NOT NULL CHECK (char_length(btrim(address)) BETWEEN 1 AND 300),
    source text GENERATED ALWAYS AS ('self-reported'::text) STORED,
    idempotency_key text NOT NULL CHECK (octet_length(idempotency_key) BETWEEN 1 AND 128),
    reported_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK (starts_at < ends_at AND ends_at <= reported_at),
    UNIQUE (organization_id, reported_by, idempotency_key)
);

CREATE INDEX organization_event_history_timeline_idx ON organization_event_history
    (organization_id, starts_at DESC, id DESC);
