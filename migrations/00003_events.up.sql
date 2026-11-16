CREATE TABLE events (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL REFERENCES organizations (id),
    created_by uuid NOT NULL REFERENCES users (id),
    name text NOT NULL CHECK (char_length(btrim(name)) BETWEEN 1 AND 160),
    slug text NOT NULL UNIQUE CHECK (char_length(slug) <= 120 AND slug ~ '^[a-z0-9]+(-[a-z0-9]+)*$'),
    status text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'published', 'live', 'completed', 'cancelled')),
    starts_at timestamptz NOT NULL,
    ends_at timestamptz NOT NULL,
    timezone text NOT NULL CHECK (char_length(timezone) BETWEEN 1 AND 64),
    city text NOT NULL CHECK (char_length(btrim(city)) BETWEEN 1 AND 120),
    address text NOT NULL CHECK (char_length(btrim(address)) BETWEEN 1 AND 300),
    description text,
    facilities text[] NOT NULL DEFAULT '{}',
    terms text,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK (ends_at > starts_at)
);

CREATE INDEX events_organization_idx ON events (organization_id, starts_at DESC, id DESC);

CREATE TABLE event_slots (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id uuid NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    code text NOT NULL CHECK (char_length(btrim(code)) BETWEEN 1 AND 100),
    starts_at timestamptz NOT NULL,
    ends_at timestamptz NOT NULL,
    CHECK (ends_at > starts_at),
    UNIQUE (event_id, code),
    UNIQUE (event_id, id)
);

CREATE TABLE event_status_history (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    event_id uuid NOT NULL REFERENCES events (id),
    actor_id uuid REFERENCES users (id),
    from_status text CHECK (from_status IN ('draft', 'published', 'live', 'completed', 'cancelled')),
    to_status text NOT NULL CHECK (to_status IN ('draft', 'published', 'live', 'completed', 'cancelled')),
    reason text,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK (from_status IS DISTINCT FROM to_status)
);

CREATE INDEX event_status_history_event_idx ON event_status_history (event_id, id DESC);
