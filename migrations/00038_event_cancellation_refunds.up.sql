CREATE TABLE event_cancellation_refunds (
    booking_id uuid PRIMARY KEY REFERENCES bookings (id),
    payment_attempt_id uuid NOT NULL UNIQUE REFERENCES payment_attempts (id),
    requested_by uuid NOT NULL REFERENCES users (id),
    amount bigint NOT NULL CHECK (amount BETWEEN 1 AND 9007199254740991),
    refund_policy_version text NOT NULL,
    requested_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE FUNCTION cancel_event_bookings(event_id uuid, actor_id uuid)
RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM public.events e JOIN public.memberships m ON m.organization_id = e.organization_id
        WHERE e.id = $1 AND e.status = 'cancelled' AND m.user_id = $2 AND m.role = 'owner'
    ) THEN
        RETURN false;
    END IF;

    -- Keep the event → booking lock order used by holds. Payment settlement locks
    -- the booking before confirming it; a later settlement follows its late-payment path.
    PERFORM b.id FROM public.bookings b WHERE b.event_id = $1 ORDER BY b.id FOR UPDATE OF b;

    IF EXISTS (
        SELECT 1 FROM public.bookings b LEFT JOIN public.allocations a ON a.booking_id = b.id
        WHERE b.event_id = $1 AND (
            a.status IS NULL
            OR (b.status IN ('held', 'payment_pending') AND a.status NOT IN ('hold', 'offer'))
            OR (b.status = 'confirmed' AND a.status <> 'reserved')
            OR (b.status IN ('expired', 'cancelled') AND a.status <> 'released')
        )
    ) THEN
        RETURN false;
    END IF;

    INSERT INTO public.event_cancellation_refunds
        (booking_id, payment_attempt_id, requested_by, amount, refund_policy_version)
    SELECT b.id, p.id, $2, b.rent / 2, b.refund_policy_version
    FROM public.bookings b
    JOIN public.payment_attempts p ON p.booking_id = b.id AND p.status = 'paid'
    JOIN public.ledger_transactions ledger ON ledger.ref_id = p.id
        AND ledger.kind = 'payment_received' AND ledger.ref_type = 'payment_attempt'
    WHERE b.event_id = $1 AND b.status = 'confirmed' AND b.rent / 2 > 0
    ON CONFLICT (booking_id) DO NOTHING;

    IF EXISTS (
        SELECT 1 FROM public.bookings b
        WHERE b.event_id = $1 AND b.status = 'confirmed' AND b.rent / 2 > 0
        AND NOT EXISTS (SELECT 1 FROM public.event_cancellation_refunds r WHERE r.booking_id = b.id)
    ) THEN
        RETURN false;
    END IF;

    WITH affected AS MATERIALIZED (
        SELECT b.id, b.status FROM public.bookings b
        WHERE b.event_id = $1 AND b.status IN ('held', 'payment_pending', 'confirmed')
    ), closed AS (
        UPDATE public.bookings b SET status = 'cancelled' FROM affected
        WHERE b.id = affected.id RETURNING b.id, affected.status
    ), released AS (
        UPDATE public.allocations a SET status = 'released' FROM closed
        WHERE a.booking_id = closed.id RETURNING a.booking_id, closed.status
    )
    INSERT INTO public.booking_status_history (booking_id, actor_id, from_status, to_status, reason)
    SELECT booking_id, $2, status, 'cancelled', 'event_cancelled' FROM released;

    RETURN true;
END;
$$;

REVOKE ALL ON FUNCTION cancel_event_bookings(uuid, uuid) FROM PUBLIC;
