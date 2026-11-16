BEGIN;
ALTER TABLE users
    ADD COLUMN IF NOT EXISTS email_verified_at
    timestamptz;

CREATE TABLE IF NOT EXISTS auth_action_tokens (
    token_hash bytea PRIMARY KEY,
    user_id  uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    purpose text NOT NULL CHECK (purpose IN ('email_verification', 'password_reset')),
    expires_at timestamptz NOT NULL,
    used_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);



CREATE INDEX IF NOT EXISTS auth_action_tokens_user_purpose_idx
    ON auth_action_tokens (user_id, purpose)
    WHERE used_at IS NULL;


CREATE INDEX IF NOT EXISTS auth_action_tokens_expires_idx
    ON auth_action_tokens (expires_at);

COMMIT;