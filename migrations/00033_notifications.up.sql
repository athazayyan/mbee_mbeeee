-- Notifikasi (E15). Worker mengumpulkan peristiwa dari tabel sumber ke outbox, satu baris per peristiwa,
-- lalu mengirimkannya ke inbox setiap penerima. Unique key menjaga peristiwa dan kiriman tidak pernah ganda.
CREATE TABLE notification_outbox (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind text NOT NULL CHECK (kind IN ('event_cancelled', 'booking_expired')),
    source_ref text NOT NULL CHECK (octet_length(source_ref) BETWEEN 1 AND 200),
    booking_id uuid REFERENCES bookings (id),
    event_id uuid REFERENCES events (id),
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'delivered', 'failed')),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    last_error text CHECK (last_error IS NULL OR char_length(last_error) <= 200),
    recipients integer CHECK (recipients IS NULL OR recipients >= 0),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    delivered_at timestamptz,
    UNIQUE (kind, source_ref),
    CHECK ((status = 'delivered') = (delivered_at IS NOT NULL))
);

CREATE INDEX notification_outbox_due_idx ON notification_outbox (next_attempt_at, id) WHERE status = 'pending';

CREATE TABLE notifications (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    outbox_id bigint NOT NULL REFERENCES notification_outbox (id),
    kind text NOT NULL,
    title text NOT NULL CHECK (char_length(title) BETWEEN 1 AND 120),
    body text NOT NULL CHECK (char_length(body) BETWEEN 1 AND 500),
    link text CHECK (link IS NULL OR (link LIKE '/%' AND link NOT LIKE '//%' AND char_length(link) <= 300)),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    read_at timestamptz,
    UNIQUE (user_id, outbox_id)
);

CREATE INDEX notifications_inbox_idx ON notifications (user_id, created_at DESC, id DESC);
CREATE INDEX notifications_unread_idx ON notifications (user_id) WHERE read_at IS NULL;
