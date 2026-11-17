-- Registration asks who is signing up. The business or organization is created with the account;
-- this row keeps the phone number and the first event name an organizer intends to create.
CREATE TABLE signup_profiles (
    user_id uuid PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    role text NOT NULL CHECK (role IN ('tenant', 'organizer')),
    phone text NOT NULL CHECK (phone ~ '^\+628[0-9]{8,12}$'),
    event_name text CHECK (event_name IS NULL OR char_length(btrim(event_name)) BETWEEN 1 AND 160),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK ((role = 'organizer') = (event_name IS NOT NULL))
);
