CREATE TABLE payment_attempts (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    booking_id uuid NOT NULL REFERENCES bookings (id),
    created_by uuid NOT NULL REFERENCES users (id),
    provider text NOT NULL CHECK (provider IN ('midtrans')),
    method text NOT NULL CHECK (char_length(btrim(method)) BETWEEN 1 AND 40),
    status text NOT NULL CHECK (status IN ('created', 'pending', 'paid', 'failed', 'expired')),
    amount bigint NOT NULL CHECK (amount BETWEEN 1 AND 9007199254740991),
    currency text NOT NULL DEFAULT 'IDR' CHECK (currency = 'IDR'),
    provider_token text,
    checkout_url text,
    expires_at timestamptz NOT NULL,
    idempotency_key text NOT NULL CHECK (octet_length(idempotency_key) BETWEEN 1 AND 128),
    request_digest bytea NOT NULL CHECK (octet_length(request_digest) = 32),
    paid_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (created_by, idempotency_key)
);

CREATE INDEX payment_attempts_booking_idx ON payment_attempts (booking_id, created_at DESC);
-- At most one live attempt per booking; a new one needs the previous to end.
CREATE UNIQUE INDEX payment_attempts_one_live_idx ON payment_attempts (booking_id)
    WHERE status IN ('created', 'pending');

-- Every provider notification is stored before it is acknowledged. dedupe_key makes retries no-ops.
CREATE TABLE payment_events (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    provider text NOT NULL,
    dedupe_key text NOT NULL,
    attempt_id uuid REFERENCES payment_attempts (id),
    provider_status text NOT NULL,
    outcome text NOT NULL CHECK (outcome IN ('applied', 'ignored', 'rejected')),
    payload jsonb NOT NULL,
    received_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (provider, dedupe_key)
);

CREATE TABLE payment_exceptions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    attempt_id uuid REFERENCES payment_attempts (id),
    booking_id uuid REFERENCES bookings (id),
    kind text NOT NULL CHECK (kind IN ('late_payment', 'duplicate_payment', 'amount_mismatch', 'unknown_order')),
    detail jsonb NOT NULL DEFAULT '{}'::jsonb,
    status text NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'resolved')),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX payment_exceptions_open_idx ON payment_exceptions (created_at) WHERE status = 'open';

CREATE TABLE ledger_transactions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind text NOT NULL CHECK (kind IN ('payment_received', 'reversal')),
    ref_type text NOT NULL,
    ref_id uuid NOT NULL,
    reverses uuid REFERENCES ledger_transactions (id),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (kind, ref_type, ref_id)
);

CREATE TABLE ledger_entries (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    transaction_id uuid NOT NULL REFERENCES ledger_transactions (id),
    account text NOT NULL CHECK (account IN
        ('provider_clearing', 'organizer_escrow', 'platform_revenue', 'provider_fee_payable')),
    direction text NOT NULL CHECK (direction IN ('debit', 'credit')),
    amount bigint NOT NULL CHECK (amount BETWEEN 1 AND 9007199254740991)
);

CREATE INDEX ledger_entries_tx_idx ON ledger_entries (transaction_id);

-- Append-only: corrections are new reversing transactions.
CREATE FUNCTION ledger_forbid_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'ledger rows are append-only';
END $$;

CREATE TRIGGER ledger_transactions_immutable BEFORE UPDATE OR DELETE ON ledger_transactions
    FOR EACH ROW EXECUTE FUNCTION ledger_forbid_change();
CREATE TRIGGER ledger_entries_immutable BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_forbid_change();

-- Every transaction must balance when committed.
CREATE FUNCTION ledger_check_balance() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE diff bigint;
BEGIN
    SELECT COALESCE(SUM(CASE direction WHEN 'debit' THEN amount ELSE -amount END), 0) INTO diff
    FROM ledger_entries WHERE transaction_id = NEW.transaction_id;
    IF diff <> 0 THEN
        RAISE EXCEPTION 'ledger transaction % is unbalanced by %', NEW.transaction_id, diff;
    END IF;
    RETURN NULL;
END $$;

CREATE CONSTRAINT TRIGGER ledger_entries_balanced AFTER INSERT ON ledger_entries
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION ledger_check_balance();
