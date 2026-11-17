-- E09: check-in, refund, sengketa, dan pencairan dana.

-- Hari non-kerja tambahan (libur nasional) untuk hitungan H+4 hari kerja. Akhir pekan sudah dihitung di kode.
CREATE TABLE non_working_days (
    day date PRIMARY KEY,
    note text NOT NULL DEFAULT '' CHECK (char_length(note) <= 100)
);

-- Kode booking untuk QR dan pencarian manual staf. Dibuat saat tenant pertama kali membukanya.
CREATE TABLE booking_codes (
    booking_id uuid PRIMARY KEY REFERENCES bookings (id),
    code text NOT NULL UNIQUE CHECK (code ~ '^[A-HJ-NP-Z2-9]{10}$'),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

-- Satu check-in per booking. Mengulang check-in mengembalikan catatan yang sama.
CREATE TABLE check_ins (
    booking_id uuid PRIMARY KEY REFERENCES bookings (id),
    event_id uuid NOT NULL REFERENCES events (id),
    checked_in_by uuid NOT NULL REFERENCES users (id),
    method text NOT NULL CHECK (method IN ('qr', 'manual')),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX check_ins_event_idx ON check_ins (event_id, created_at DESC);

CREATE TABLE refunds (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    booking_id uuid NOT NULL REFERENCES bookings (id),
    event_id uuid NOT NULL REFERENCES events (id),
    attempt_id uuid NOT NULL REFERENCES payment_attempts (id),
    reason text NOT NULL CHECK (reason IN ('event_cancelled', 'dispute')),
    amount bigint NOT NULL CHECK (amount BETWEEN 1 AND 9007199254740991),
    rent_amount bigint NOT NULL CHECK (rent_amount >= amount),
    -- Biaya pemrosesan refund dari provider. Ditanggung penyelenggara.
    fee_amount bigint NOT NULL DEFAULT 0 CHECK (fee_amount BETWEEN 0 AND 9007199254740991),
    -- api: refund lewat provider. manual: dikembalikan ke rekening tenant oleh operator.
    mode text NOT NULL CHECK (mode IN ('api', 'manual')),
    status text NOT NULL CHECK (status IN ('requested', 'manual_required', 'refunded', 'failed')),
    provider_ref text CHECK (provider_ref IS NULL OR char_length(provider_ref) <= 100),
    failure text CHECK (failure IS NULL OR char_length(failure) <= 300),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    refunded_at timestamptz,
    UNIQUE (booking_id, reason),
    CHECK ((status = 'refunded') = (refunded_at IS NOT NULL))
);
CREATE INDEX refunds_event_idx ON refunds (event_id);
CREATE INDEX refunds_open_idx ON refunds (created_at) WHERE status IN ('requested', 'manual_required', 'failed');

CREATE TABLE organization_bank_accounts (
    organization_id uuid PRIMARY KEY REFERENCES organizations (id),
    bank_code text NOT NULL CHECK (bank_code ~ '^[a-z0-9_]{2,20}$'),
    account_number text NOT NULL CHECK (account_number ~ '^[0-9]{6,20}$'),
    holder_name text NOT NULL CHECK (char_length(btrim(holder_name)) BETWEEN 1 AND 100),
    updated_by uuid NOT NULL REFERENCES users (id),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE payouts (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id uuid NOT NULL REFERENCES events (id),
    organization_id uuid NOT NULL REFERENCES organizations (id),
    amount bigint NOT NULL CHECK (amount BETWEEN 1 AND 9007199254740991),
    status text NOT NULL CHECK (status IN ('requested', 'released', 'failed')),
    requested_by uuid NOT NULL REFERENCES users (id),
    -- Rekening disalin saat pengajuan, sehingga perubahan rekening tidak mengubah tujuan pencairan yang berjalan.
    bank_code text NOT NULL,
    account_number text NOT NULL,
    holder_name text NOT NULL,
    idempotency_key text NOT NULL CHECK (octet_length(idempotency_key) BETWEEN 1 AND 128),
    provider_ref text CHECK (provider_ref IS NULL OR char_length(provider_ref) <= 100),
    failure text CHECK (failure IS NULL OR char_length(failure) <= 300),
    requested_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    released_at timestamptz,
    UNIQUE (requested_by, idempotency_key),
    CHECK ((status = 'released') = (released_at IS NOT NULL))
);
-- Satu pencairan aktif atau selesai per acara. Pencairan gagal boleh diajukan ulang.
CREATE UNIQUE INDEX payouts_event_live_idx ON payouts (event_id) WHERE status IN ('requested', 'released');

CREATE TABLE disputes (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    booking_id uuid NOT NULL REFERENCES bookings (id),
    event_id uuid NOT NULL REFERENCES events (id),
    opened_by uuid NOT NULL REFERENCES users (id),
    reason text NOT NULL CHECK (char_length(btrim(reason)) BETWEEN 1 AND 2000),
    status text NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'refunded', 'rejected')),
    resolution_note text CHECK (resolution_note IS NULL OR char_length(resolution_note) <= 2000),
    refund_amount bigint CHECK (refund_amount IS NULL OR refund_amount > 0),
    resolved_by uuid REFERENCES users (id),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    resolved_at timestamptz,
    CHECK ((status = 'open') = (resolved_at IS NULL))
);
CREATE UNIQUE INDEX disputes_open_idx ON disputes (booking_id) WHERE status = 'open';
CREATE INDEX disputes_event_idx ON disputes (event_id, status);

-- Transaksi provider yang harus ditutup setelah pembatalan acara. Penutupan terjadi di luar transaksi pembatalan.
CREATE TABLE payment_closures (
    attempt_id uuid PRIMARY KEY REFERENCES payment_attempts (id),
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'done')),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    done_at timestamptz
);
CREATE INDEX payment_closures_pending_idx ON payment_closures (created_at) WHERE status = 'pending';

-- Ledger: dana yang sudah dialokasikan untuk refund dan pencairan tetapi belum keluar dari provider.
ALTER TABLE ledger_entries DROP CONSTRAINT ledger_entries_account_check;
ALTER TABLE ledger_entries ADD CONSTRAINT ledger_entries_account_check CHECK (account IN
    ('provider_clearing', 'organizer_escrow', 'platform_revenue', 'provider_fee_payable', 'refund_payable', 'payout_payable'));
ALTER TABLE ledger_transactions DROP CONSTRAINT ledger_transactions_kind_check;
ALTER TABLE ledger_transactions ADD CONSTRAINT ledger_transactions_kind_check CHECK (kind IN
    ('payment_received', 'plan_payment_received', 'reversal', 'refund_reserved', 'refund_issued',
     'payout_reserved', 'payout_released'));
ALTER TABLE payment_exceptions DROP CONSTRAINT payment_exceptions_kind_check;
ALTER TABLE payment_exceptions ADD CONSTRAINT payment_exceptions_kind_check CHECK (kind IN
    ('late_payment', 'duplicate_payment', 'amount_mismatch', 'unknown_order', 'method_mismatch',
     'ledger_missing', 'ledger_mismatch', 'manual_refund_required', 'refund_failed'));
