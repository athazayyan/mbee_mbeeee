ALTER TABLE event_cancellation_refunds
    ADD COLUMN status text NOT NULL DEFAULT 'queued'
        CHECK (status IN ('queued', 'submitted', 'confirmed', 'manual_review')),
    ADD COLUMN next_check_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    ADD COLUMN confirmed_at timestamptz,
    ADD CONSTRAINT event_cancellation_refund_confirmation_check
        CHECK ((status = 'confirmed') = (confirmed_at IS NOT NULL));

CREATE INDEX event_cancellation_refunds_due_idx
    ON event_cancellation_refunds (next_check_at, booking_id)
    WHERE status IN ('queued', 'submitted');

ALTER TABLE ledger_transactions DROP CONSTRAINT ledger_transactions_kind_check;
ALTER TABLE ledger_transactions ADD CONSTRAINT ledger_transactions_kind_check
    CHECK (kind IN ('payment_received', 'plan_payment_received', 'event_cancellation_refund', 'reversal'));
