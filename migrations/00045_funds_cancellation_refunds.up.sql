-- Refunds for a cancelled event live in event_cancellation_refunds (00038-00042). The funds module keeps
-- its own refunds table for dispute decisions only, so the cancellation-time closure queue is not needed.
DROP TABLE payment_closures;

-- 00041 and 00042 rewrote the ledger checks without the funds accounts and kinds. Install the union.
ALTER TABLE ledger_entries DROP CONSTRAINT ledger_entries_account_check;
ALTER TABLE ledger_entries ADD CONSTRAINT ledger_entries_account_check CHECK (account IN
    ('provider_clearing', 'organizer_escrow', 'platform_revenue', 'provider_fee_payable', 'manual_refund_cash',
     'refund_payable', 'payout_payable'));
ALTER TABLE ledger_transactions DROP CONSTRAINT ledger_transactions_kind_check;
ALTER TABLE ledger_transactions ADD CONSTRAINT ledger_transactions_kind_check CHECK (kind IN
    ('payment_received', 'plan_payment_received', 'reversal', 'event_cancellation_refund',
     'refund_reserved', 'refund_issued', 'payout_reserved', 'payout_released'));
