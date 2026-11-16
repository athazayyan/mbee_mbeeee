-- E07: curated registration. Direct booking stays the default.
ALTER TABLE events ADD COLUMN registration_mode text NOT NULL DEFAULT 'direct'
    CHECK (registration_mode IN ('direct', 'curated'));

CREATE TABLE applications (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id uuid NOT NULL REFERENCES events (id),
    business_id uuid NOT NULL REFERENCES businesses (id),
    submitted_by uuid NOT NULL REFERENCES users (id),
    status text NOT NULL DEFAULT 'submitted' CHECK (status IN ('submitted', 'approved', 'rejected', 'withdrawn', 'expired')),
    preferred_stand_ids uuid[] NOT NULL DEFAULT '{}' CHECK (cardinality(preferred_stand_ids) <= 5),
    reason text CHECK (reason IS NULL OR char_length(btrim(reason)) BETWEEN 1 AND 500),
    booking_id uuid UNIQUE REFERENCES bookings (id),
    decided_by uuid REFERENCES users (id),
    decided_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK (status <> 'approved' OR booking_id IS NOT NULL),
    CHECK (status IN ('submitted', 'withdrawn') OR decided_by IS NOT NULL)
);

-- One live application per business and event. A rejected or lapsed one allows a fresh submission.
CREATE UNIQUE INDEX applications_live_idx ON applications (event_id, business_id)
    WHERE status IN ('submitted', 'approved');
CREATE INDEX applications_event_idx ON applications (event_id, created_at DESC, id DESC);
CREATE INDEX applications_business_idx ON applications (business_id, created_at DESC, id DESC);

CREATE TABLE application_status_history (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    application_id uuid NOT NULL REFERENCES applications (id),
    actor_id uuid REFERENCES users (id),
    from_status text CHECK (from_status IN ('submitted', 'approved', 'rejected', 'withdrawn', 'expired')),
    to_status text NOT NULL CHECK (to_status IN ('submitted', 'approved', 'rejected', 'withdrawn', 'expired')),
    reason text,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK (from_status IS DISTINCT FROM to_status)
);

CREATE INDEX application_status_history_idx ON application_status_history (application_id, id DESC);
