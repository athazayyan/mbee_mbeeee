CREATE TABLE bookings (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id uuid NOT NULL REFERENCES businesses (id),
    event_id uuid NOT NULL REFERENCES events (id),
    category_id uuid NOT NULL REFERENCES business_categories (id),
    created_by uuid NOT NULL REFERENCES users (id),
    status text NOT NULL CHECK (status IN ('held', 'payment_pending', 'confirmed', 'expired', 'cancelled')),
    expires_at timestamptz NOT NULL,
    price_revision integer NOT NULL,
    rent bigint NOT NULL CHECK (rent BETWEEN 0 AND 9007199254740991),
    platform_fee bigint NOT NULL CHECK (platform_fee BETWEEN 0 AND 9007199254740991),
    provider_fee bigint NOT NULL CHECK (provider_fee BETWEEN 0 AND 9007199254740991),
    total bigint GENERATED ALWAYS AS (rent + platform_fee + provider_fee) STORED CHECK (total <= 9007199254740991),
    terms_snapshot text NOT NULL CHECK (char_length(btrim(terms_snapshot)) > 0),
    refund_policy_version text NOT NULL CHECK (char_length(btrim(refund_policy_version)) BETWEEN 1 AND 80),
    refund_policy_snapshot jsonb NOT NULL CHECK (jsonb_typeof(refund_policy_snapshot) = 'object'),
    idempotency_key text,
    request_digest bytea,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK (
        (idempotency_key IS NULL AND request_digest IS NULL)
        OR (idempotency_key IS NOT NULL AND octet_length(idempotency_key) BETWEEN 1 AND 128
            AND request_digest IS NOT NULL AND octet_length(request_digest) = 32)
    ),
    FOREIGN KEY (event_id, price_revision) REFERENCES event_pricing_versions (event_id, revision),
    UNIQUE (id, event_id)
);

CREATE UNIQUE INDEX bookings_actor_idempotency_idx ON bookings (created_by, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
CREATE INDEX bookings_business_idx ON bookings (business_id, created_at DESC, id DESC);
CREATE INDEX bookings_active_expiry_idx ON bookings (expires_at, id) WHERE status IN ('held', 'payment_pending');
CREATE INDEX bookings_actor_active_idx ON bookings (created_by, expires_at) WHERE status IN ('held', 'payment_pending');
CREATE INDEX bookings_event_category_idx ON bookings (event_id, category_id)
    WHERE status IN ('held', 'payment_pending', 'confirmed');

CREATE TABLE allocations (
    booking_id uuid PRIMARY KEY,
    event_id uuid NOT NULL,
    event_slot_id uuid NOT NULL,
    stand_id uuid NOT NULL,
    block_id uuid,
    status text NOT NULL CHECK (status IN ('hold', 'offer', 'reserved', 'released')),
    FOREIGN KEY (booking_id, event_id) REFERENCES bookings (id, event_id),
    FOREIGN KEY (event_id, event_slot_id) REFERENCES event_slots (event_id, id)
);

-- The stable stand foreign key is added alongside the E05 stand table.
CREATE UNIQUE INDEX allocations_active_stand_slot_idx ON allocations (stand_id, event_slot_id)
    WHERE status IN ('hold', 'offer', 'reserved');

CREATE TABLE booking_status_history (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    booking_id uuid NOT NULL REFERENCES bookings (id),
    actor_id uuid REFERENCES users (id),
    from_status text CHECK (from_status IN ('held', 'payment_pending', 'confirmed', 'expired', 'cancelled')),
    to_status text NOT NULL CHECK (to_status IN ('held', 'payment_pending', 'confirmed', 'expired', 'cancelled')),
    reason text,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK (from_status IS DISTINCT FROM to_status)
);

CREATE INDEX booking_status_history_booking_idx ON booking_status_history (booking_id, id DESC);
