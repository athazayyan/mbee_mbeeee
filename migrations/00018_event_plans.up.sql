-- E16-04: paid plan per event. Gating at publish is opt-in (EVENT_PLAN_REQUIRED) so existing events keep working.
CREATE TABLE event_plans (
    code text PRIMARY KEY CHECK (code ~ '^[a-z0-9]+(-[a-z0-9]+)*$' AND char_length(code) <= 40),
    name text NOT NULL CHECK (char_length(btrim(name)) BETWEEN 1 AND 80),
    price bigint NOT NULL CHECK (price BETWEEN 0 AND 9007199254740991),
    capacity_stands integer CHECK (capacity_stands IS NULL OR capacity_stands >= 1),
    active boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

-- Price 0 activates without a payment. Set the real price before turning the requirement on in production.
INSERT INTO event_plans (code, name, price, capacity_stands) VALUES ('standard', 'Paket Standar', 0, NULL);

CREATE TABLE event_plan_purchases (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id uuid NOT NULL REFERENCES events (id),
    plan_code text NOT NULL REFERENCES event_plans (code),
    purchased_by uuid NOT NULL REFERENCES users (id),
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'active')),
    amount bigint NOT NULL CHECK (amount BETWEEN 0 AND 9007199254740991),
    capacity_stands integer CHECK (capacity_stands IS NULL OR capacity_stands >= 1),
    activated_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK ((status = 'active') = (activated_at IS NOT NULL))
);

-- One open purchase and at most one active plan per event.
CREATE UNIQUE INDEX event_plan_purchases_pending_idx ON event_plan_purchases (event_id) WHERE status = 'pending';
CREATE UNIQUE INDEX event_plan_purchases_active_idx ON event_plan_purchases (event_id) WHERE status = 'active';

-- Payment attempts now belong to either a booking (rent) or a plan purchase.
ALTER TABLE payment_attempts ALTER COLUMN booking_id DROP NOT NULL;
ALTER TABLE payment_attempts ADD COLUMN purchase_id uuid REFERENCES event_plan_purchases (id);
ALTER TABLE payment_attempts ADD CONSTRAINT payment_attempts_one_subject CHECK (num_nonnulls(booking_id, purchase_id) = 1);
CREATE UNIQUE INDEX payment_attempts_one_live_purchase_idx ON payment_attempts (purchase_id)
    WHERE purchase_id IS NOT NULL AND status IN ('created', 'pending');
CREATE INDEX payment_attempts_purchase_idx ON payment_attempts (purchase_id, created_at DESC) WHERE purchase_id IS NOT NULL;

-- Plan revenue is platform income, not organizer escrow, so it gets its own ledger kind.
ALTER TABLE ledger_transactions DROP CONSTRAINT ledger_transactions_kind_check;
ALTER TABLE ledger_transactions ADD CONSTRAINT ledger_transactions_kind_check
    CHECK (kind IN ('payment_received', 'plan_payment_received', 'reversal'));
