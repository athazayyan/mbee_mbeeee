BEGIN;

CREATE TABLE users (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email text NOT NULL UNIQUE,
    password_hash bytea NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (email = lower(email))
);

CREATE TABLE sessions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz
);

CREATE INDEX sessions_expires_at_idx ON sessions (expires_at);

CREATE TABLE session_refresh_tokens (
    token_hash bytea PRIMARY KEY,
    session_id uuid NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    used_at timestamptz
);

COMMIT;
