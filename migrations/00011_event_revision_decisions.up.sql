CREATE TABLE event_revision_decisions (
    revision_id uuid NOT NULL,
    booking_id uuid NOT NULL,
    decided_by uuid NOT NULL REFERENCES users (id),
    decision text NOT NULL CHECK (decision IN ('approved', 'declined')),
    decided_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (revision_id, booking_id),
    FOREIGN KEY (revision_id, booking_id) REFERENCES event_revision_bookings (revision_id, booking_id)
);
