ALTER TABLE payment_attempts ADD COLUMN cancellation_checked_at timestamptz;

CREATE INDEX payment_attempts_cancellation_lookup_idx
    ON payment_attempts (booking_id, cancellation_checked_at)
    WHERE booking_id IS NOT NULL AND status IN ('created', 'pending');
