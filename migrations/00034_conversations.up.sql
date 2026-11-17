-- Chat tenant–penyelenggara (E15). Satu percakapan per acara dan usaha tenant, dibuka dari booking.
-- Anggota usaha dan anggota organisasi pemilik acara adalah peserta; pihak lain tidak bisa membaca.
CREATE TABLE conversations (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id uuid NOT NULL REFERENCES events (id),
    business_id uuid NOT NULL REFERENCES businesses (id),
    organization_id uuid NOT NULL REFERENCES organizations (id),
    booking_id uuid NOT NULL REFERENCES bookings (id),
    created_by uuid NOT NULL REFERENCES users (id),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    last_message_at timestamptz,
    UNIQUE (event_id, business_id)
);

CREATE INDEX conversations_business_idx ON conversations (business_id, last_message_at DESC NULLS LAST);
CREATE INDEX conversations_organization_idx ON conversations (organization_id, last_message_at DESC NULLS LAST);

-- Pesan tidak pernah diubah isinya. Moderasi hanya menyembunyikan, dan alasannya tercatat di admin_audit.
CREATE TABLE messages (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    conversation_id uuid NOT NULL REFERENCES conversations (id),
    sender_id uuid NOT NULL REFERENCES users (id),
    sender_side text NOT NULL CHECK (sender_side IN ('business', 'organization')),
    client_message_id uuid NOT NULL,
    body text NOT NULL CHECK (char_length(btrim(body)) BETWEEN 1 AND 4000),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    hidden_at timestamptz,
    hidden_by uuid REFERENCES users (id),
    UNIQUE (conversation_id, sender_id, client_message_id),
    CHECK ((hidden_at IS NULL) = (hidden_by IS NULL))
);

CREATE INDEX messages_conversation_idx ON messages (conversation_id, created_at DESC, id DESC);
CREATE INDEX messages_sender_recent_idx ON messages (sender_id, created_at DESC);

CREATE TABLE conversation_reads (
    conversation_id uuid NOT NULL REFERENCES conversations (id),
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    last_read_at timestamptz NOT NULL,
    PRIMARY KEY (conversation_id, user_id)
);

CREATE TABLE message_reports (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    message_id uuid NOT NULL REFERENCES messages (id),
    reporter_id uuid NOT NULL REFERENCES users (id),
    reason text NOT NULL CHECK (reason IN ('spam', 'abuse', 'fraud', 'other')),
    note text CHECK (note IS NULL OR char_length(note) <= 500),
    status text NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'actioned', 'dismissed')),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    resolved_at timestamptz,
    resolved_by uuid REFERENCES users (id),
    UNIQUE (message_id, reporter_id),
    CHECK ((status = 'open') = (resolved_at IS NULL))
);

CREATE INDEX message_reports_open_idx ON message_reports (created_at, id) WHERE status = 'open';
