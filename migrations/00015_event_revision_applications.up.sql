CREATE TABLE event_revision_applications (
    revision_id uuid PRIMARY KEY,
    event_id uuid NOT NULL,
    applied_by uuid NOT NULL REFERENCES users (id),
    applied_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    supersedes_through integer NOT NULL CHECK (supersedes_through > 0),
    FOREIGN KEY (revision_id, event_id) REFERENCES event_revisions (id, event_id)
);

CREATE INDEX event_revision_applications_event_idx ON event_revision_applications (event_id, supersedes_through DESC);
