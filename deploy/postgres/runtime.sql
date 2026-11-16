\set ON_ERROR_STOP on
-- Apply as the database and table owner after migrations in a dedicated TenantKu database.
-- Supply two dedicated PostgreSQL login roles through -v api_role=... -v worker_role=....
BEGIN;

SELECT current_database() AS database_name,
       (:'api_role' <> :'worker_role' AND
        (SELECT count(*) = 2
         FROM pg_roles r
         WHERE r.rolname IN (:'api_role', :'worker_role') AND r.rolcanlogin
           AND NOT (r.rolsuper OR r.rolcreaterole OR r.rolcreatedb OR r.rolreplication OR r.rolbypassrls)
           AND r.rolname <> current_user
           AND NOT pg_has_role(r.oid, (SELECT datdba FROM pg_database WHERE datname = current_database()), 'MEMBER'))
       ) AS valid_runtime_roles
\gset
\if :valid_runtime_roles
\else
DO $$ BEGIN RAISE EXCEPTION 'Use two distinct login roles without elevated privileges or ownership'; END $$;
\endif

REVOKE CONNECT, TEMP ON DATABASE :"database_name" FROM PUBLIC;
REVOKE ALL ON DATABASE :"database_name" FROM :"api_role", :"worker_role";
GRANT CONNECT ON DATABASE :"database_name" TO :"api_role", :"worker_role";
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
REVOKE ALL ON SCHEMA public FROM :"api_role", :"worker_role";
GRANT USAGE ON SCHEMA public TO :"api_role", :"worker_role";
REVOKE ALL ON ALL TABLES IN SCHEMA public FROM :"api_role", :"worker_role";
REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM :"api_role", :"worker_role";

GRANT SELECT ON public.business_categories, public.businesses, public.organizations, public.memberships,
    public.consent_events, public.events, public.event_slots, public.event_status_history,
    public.event_pricing_versions TO :"api_role";
GRANT SELECT (id, email, password_hash) ON public.users TO :"api_role";
GRANT SELECT (id, user_id, expires_at, revoked_at) ON public.sessions TO :"api_role";
GRANT SELECT (token_hash, session_id, used_at) ON public.session_refresh_tokens TO :"api_role";
GRANT SELECT (id, business_id, event_id, status, expires_at, created_at, refund_policy_version,
    price_revision, rent, platform_fee, provider_fee, total) ON public.bookings TO :"api_role";
GRANT SELECT (booking_id, stand_id, event_slot_id) ON public.allocations TO :"api_role";
GRANT SELECT (id) ON public.booking_status_history TO :"api_role";
GRANT INSERT (email, password_hash) ON public.users TO :"api_role";
GRANT INSERT (user_id, expires_at), UPDATE (revoked_at) ON public.sessions TO :"api_role";
GRANT INSERT (session_id, token_hash), UPDATE (used_at) ON public.session_refresh_tokens TO :"api_role";
GRANT INSERT (owner_id, name, category_id), UPDATE (name, category_id) ON public.businesses TO :"api_role";
GRANT INSERT (owner_id, name), UPDATE (name) ON public.organizations TO :"api_role";
GRANT INSERT (user_id, business_id, organization_id, role) ON public.memberships TO :"api_role";
-- PostgreSQL row locks require UPDATE on at least one column of the locked table.
GRANT UPDATE (id) ON public.memberships TO :"api_role";
GRANT INSERT (business_id, actor_id, purpose, granted, policy_version) ON public.consent_events TO :"api_role";
GRANT INSERT (organization_id, created_by, name, slug, starts_at, ends_at, timezone,
    city, address, description, facilities, terms),
    UPDATE (name, slug, starts_at, ends_at, timezone, city, address, description,
    facilities, terms, status, updated_at) ON public.events TO :"api_role";
GRANT INSERT (event_id, code, starts_at, ends_at), UPDATE (starts_at, ends_at) ON public.event_slots TO :"api_role";
GRANT INSERT (event_id, actor_id, from_status, to_status, reason) ON public.event_status_history TO :"api_role";
GRANT INSERT (event_id, revision, created_by, rules, effective_at) ON public.event_pricing_versions TO :"api_role";

GRANT SELECT (id, status, expires_at, event_id), UPDATE (status) ON public.bookings TO :"worker_role";
GRANT SELECT (booking_id, status), UPDATE (status) ON public.allocations TO :"worker_role";
GRANT SELECT (id), INSERT (booking_id, from_status, to_status, reason)
    ON public.booking_status_history TO :"worker_role";

COMMIT;
