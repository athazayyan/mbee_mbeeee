-- Two designs of per-method fees met on main. Keep the booking breakdown columns (rent_amount,
-- platform_fee_amount, provider_fee_amount, fee_version) as the single record of what an attempt charged,
-- and retire base_amount and method_fee from 00020.

-- Attempts made before the breakdown existed get one from their booking, with the old method_fee as the provider fee.
UPDATE payment_attempts a SET rent_amount = b.rent, platform_fee_amount = b.platform_fee, provider_fee_amount = a.method_fee
    FROM bookings b WHERE b.id = a.booking_id AND a.rent_amount IS NULL;

ALTER TABLE payment_attempts DROP COLUMN base_amount, DROP COLUMN method_fee;

-- The fee table is versioned so each attempt records which tariff it was priced with.
ALTER TABLE payment_methods ADD COLUMN fee_version text NOT NULL DEFAULT 'midtrans-sandbox-v1'
    CHECK (char_length(fee_version) BETWEEN 1 AND 40);

-- Released sandbox methods are BCA Virtual Account and QRIS, priced as decided with the booking payment flow.
-- The earlier multi-bank, QRIS-percentage, and GoPay rows stay but are closed; enable them again with UPDATE
-- once their fees are confirmed (PROD-02).
UPDATE payment_methods SET enabled = false WHERE code IN ('bank_transfer', 'qris', 'gopay');
INSERT INTO payment_methods (code, label, description, snap_channels, confirm_types, fee_flat, fee_bps, refund_mode, enabled, sort_order) VALUES
    ('bca_va', 'BCA Virtual Account', 'Transfer lewat nomor virtual account BCA.', ARRAY['bca_va'], ARRAY['bank_transfer'], 4000, 0, 'manual', true, 10),
    ('other_qris', 'QRIS', 'Bayar dari aplikasi bank atau dompet digital apa pun.', ARRAY['other_qris'], ARRAY['qris'], 0, 0, 'manual', true, 20);
