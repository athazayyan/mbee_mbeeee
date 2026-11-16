CREATE TABLE event_pricing_versions (
    event_id uuid NOT NULL REFERENCES events (id),
    revision integer NOT NULL CHECK (revision > 0),
    created_by uuid NOT NULL REFERENCES users (id),
    rules jsonb NOT NULL CHECK (jsonb_typeof(rules) = 'object'),
    effective_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (event_id, revision)
);

CREATE INDEX event_pricing_effective_idx ON event_pricing_versions
    (event_id, effective_at DESC, revision DESC);
