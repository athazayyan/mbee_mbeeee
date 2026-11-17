ALTER TABLE payment_attempts
    ADD COLUMN rent_amount bigint,
    ADD COLUMN platform_fee_amount bigint,
    ADD COLUMN provider_fee_amount bigint,
    ADD CONSTRAINT payment_attempts_booking_breakdown_check CHECK (
        num_nonnulls(rent_amount, platform_fee_amount, provider_fee_amount) = 0
        OR (
            booking_id IS NOT NULL
            AND num_nonnulls(rent_amount, platform_fee_amount, provider_fee_amount) = 3
            AND rent_amount BETWEEN 0 AND amount
            AND platform_fee_amount BETWEEN 0 AND amount
            AND provider_fee_amount BETWEEN 0 AND amount
            AND rent_amount + platform_fee_amount + provider_fee_amount = amount
        )
    );
