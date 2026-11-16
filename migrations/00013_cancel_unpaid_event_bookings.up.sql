CREATE FUNCTION public.cancel_unpaid_event_bookings(event_id uuid, actor_id uuid)
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

    -- Hold creation locks the event first. Lock its bookings before examining
    -- their history so cancellation and the expiry worker cannot both close one.
    PERFORM b.id FROM public.bookings b WHERE b.event_id = $1 ORDER BY b.id FOR UPDATE OF b;

    IF EXISTS (
        SELECT 1 FROM public.bookings b LEFT JOIN public.allocations a ON a.booking_id = b.id
        WHERE b.event_id = $1 AND (
            b.status NOT IN ('held', 'expired', 'cancelled')
            OR a.status IS NULL
            OR (b.status = 'held' AND a.status <> 'hold')
            OR (b.status IN ('expired', 'cancelled') AND a.status <> 'released')
            OR EXISTS (
                SELECT 1 FROM public.booking_status_history h WHERE h.booking_id = b.id
                AND (h.from_status IN ('payment_pending', 'confirmed') OR h.to_status IN ('payment_pending', 'confirmed'))
            )
        )
    ) THEN
        RETURN false;
    END IF;

    WITH closed AS (
        UPDATE public.bookings b SET status = 'cancelled'
        WHERE b.event_id = $1 AND b.status = 'held' RETURNING b.id
    ), released AS (
        UPDATE public.allocations a SET status = 'released' FROM closed
        WHERE a.booking_id = closed.id RETURNING a.booking_id
    )
    INSERT INTO public.booking_status_history (booking_id, actor_id, from_status, to_status, reason)
    SELECT booking_id, $2, 'held', 'cancelled', 'event_cancelled' FROM released;

    RETURN true;
END;
$$;

REVOKE ALL ON FUNCTION public.cancel_unpaid_event_bookings(uuid, uuid) FROM PUBLIC;
