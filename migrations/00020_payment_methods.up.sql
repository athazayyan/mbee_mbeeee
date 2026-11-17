-- E08-01/E08-02: payment methods released through Midtrans, their admin fee, and the method bound to each attempt.
-- Fees below are public Midtrans list rates used as estimates. Confirm them against the merchant contract
-- (PROD-02) and update the rows; no code change is needed. The tenant bears the admin fee (PROD-03).
CREATE TABLE payment_methods (
    code text PRIMARY KEY CHECK (code ~ '^[a-z0-9_]+$' AND char_length(code) <= 40),
    label text NOT NULL CHECK (char_length(btrim(label)) BETWEEN 1 AND 80),
    description text NOT NULL DEFAULT '' CHECK (char_length(description) <= 200),
    -- Snap channel names sent as enabled_payments, so the hosted page offers only this method.
    snap_channels text[] NOT NULL CHECK (cardinality(snap_channels) >= 1),
    -- payment_type values Midtrans may report in notifications for this method.
    confirm_types text[] NOT NULL CHECK (cardinality(confirm_types) >= 1),
    fee_flat bigint NOT NULL DEFAULT 0 CHECK (fee_flat BETWEEN 0 AND 1000000),
    fee_bps integer NOT NULL DEFAULT 0 CHECK (fee_bps BETWEEN 0 AND 2000),
    fee_source text NOT NULL DEFAULT 'estimate' CHECK (fee_source IN ('estimate', 'contract')),
    -- api: refund through the provider; manual: pay the tenant back by bank transfer (E09).
    refund_mode text NOT NULL DEFAULT 'manual' CHECK (refund_mode IN ('api', 'manual')),
    enabled boolean NOT NULL DEFAULT false,
    sort_order integer NOT NULL DEFAULT 100,
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

INSERT INTO payment_methods (code, label, description, snap_channels, confirm_types, fee_flat, fee_bps, refund_mode, enabled, sort_order) VALUES
    ('bank_transfer', 'Transfer bank (Virtual Account)', 'BCA, BNI, BRI, Permata, atau bank lain.',
     ARRAY['bca_va', 'bni_va', 'bri_va', 'permata_va', 'other_va'], ARRAY['bank_transfer'], 4000, 0, 'manual', true, 10),
    ('qris', 'QRIS', 'Bayar dari aplikasi bank atau dompet digital apa pun.',
     ARRAY['other_qris'], ARRAY['qris'], 0, 70, 'manual', true, 20),
    ('gopay', 'GoPay', 'Bayar dari aplikasi Gojek.',
     ARRAY['gopay'], ARRAY['gopay'], 0, 200, 'api', true, 30);

ALTER TABLE payment_attempts
    ADD COLUMN base_amount bigint,
    ADD COLUMN method_fee bigint NOT NULL DEFAULT 0 CHECK (method_fee BETWEEN 0 AND 9007199254740991),
    ADD COLUMN confirmed_type text CHECK (confirmed_type IS NULL OR char_length(confirmed_type) <= 40),
    ADD COLUMN confirmed_detail text CHECK (confirmed_detail IS NULL OR char_length(confirmed_detail) <= 40);

-- Earlier attempts charged the booking total; its non-rent part becomes the method fee.
UPDATE payment_attempts a SET base_amount = b.rent + b.platform_fee, method_fee = a.amount - b.rent - b.platform_fee
    FROM bookings b WHERE b.id = a.booking_id;
UPDATE payment_attempts SET base_amount = amount WHERE purchase_id IS NOT NULL;

ALTER TABLE payment_attempts ALTER COLUMN base_amount SET NOT NULL;
ALTER TABLE payment_attempts ADD CONSTRAINT payment_attempts_amount_split
    CHECK (base_amount >= 0 AND method_fee >= 0 AND base_amount + method_fee = amount);

-- Reconciliation and manual follow-up kinds.
ALTER TABLE payment_exceptions DROP CONSTRAINT payment_exceptions_kind_check;
ALTER TABLE payment_exceptions ADD CONSTRAINT payment_exceptions_kind_check CHECK (kind IN
    ('late_payment', 'duplicate_payment', 'amount_mismatch', 'unknown_order',
     'method_mismatch', 'ledger_missing', 'ledger_mismatch'));

-- Refunds and payouts are separate ledger kinds so E09 never mixes them with received payments.
ALTER TABLE ledger_transactions DROP CONSTRAINT ledger_transactions_kind_check;
ALTER TABLE ledger_transactions ADD CONSTRAINT ledger_transactions_kind_check CHECK (kind IN
    ('payment_received', 'plan_payment_received', 'reversal', 'refund_issued', 'payout_released'));
