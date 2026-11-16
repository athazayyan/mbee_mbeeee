CREATE TABLE event_revisions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id uuid NOT NULL REFERENCES events (id),
    revision integer NOT NULL CHECK (revision > 0),
    requested_by uuid NOT NULL REFERENCES users (id),
    reason text NOT NULL CHECK (char_length(btrim(reason)) BETWEEN 1 AND 2000),
    snapshot_before jsonb NOT NULL CHECK (jsonb_typeof(snapshot_before) = 'object'),
    snapshot_after jsonb NOT NULL CHECK (jsonb_typeof(snapshot_after) = 'object'),
    idempotency_key text NOT NULL CHECK (octet_length(idempotency_key) BETWEEN 1 AND 128),
    request_digest bytea NOT NULL CHECK (octet_length(request_digest) = 32),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK (snapshot_before IS DISTINCT FROM snapshot_after),
    UNIQUE (event_id, revision),
    UNIQUE (event_id, requested_by, idempotency_key),
    UNIQUE (id, event_id)
);

CREATE INDEX event_revisions_timeline_idx ON event_revisions (event_id, created_at DESC, id DESC);

CREATE TABLE event_revision_bookings (
    revision_id uuid NOT NULL,
    event_id uuid NOT NULL,
    booking_id uuid NOT NULL,
    status_at_proposal text NOT NULL CHECK (status_at_proposal IN ('held', 'payment_pending', 'confirmed')),
    PRIMARY KEY (revision_id, booking_id),
    FOREIGN KEY (revision_id, event_id) REFERENCES event_revisions (id, event_id),
    FOREIGN KEY (booking_id, event_id) REFERENCES bookings (id, event_id)
);
