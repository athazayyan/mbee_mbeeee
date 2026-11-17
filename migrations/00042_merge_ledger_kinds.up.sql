ALTER TABLE ledger_transactions DROP CONSTRAINT ledger_transactions_kind_check;
ALTER TABLE ledger_transactions ADD CONSTRAINT ledger_transactions_kind_check
    CHECK (kind IN ('payment_received', 'plan_payment_received', 'reversal',
        'refund_issued', 'payout_released', 'event_cancellation_refund'));
