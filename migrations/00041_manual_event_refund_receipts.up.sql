CREATE TABLE manual_event_refund_receipts (
    booking_id uuid PRIMARY KEY REFERENCES event_cancellation_refunds (booking_id),
    bank_reference text NOT NULL UNIQUE CHECK (char_length(bank_reference) BETWEEN 1 AND 128),
    recorded_by name NOT NULL DEFAULT current_user,
    recorded_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

ALTER TABLE ledger_entries DROP CONSTRAINT ledger_entries_account_check;
ALTER TABLE ledger_entries ADD CONSTRAINT ledger_entries_account_check
    CHECK (account IN ('provider_clearing', 'organizer_escrow', 'platform_revenue', 'provider_fee_payable', 'manual_refund_cash'));
