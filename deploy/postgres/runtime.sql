\set ON_ERROR_STOP on
-- Apply as the database and table owner after migrations in a dedicated TenantKu database.
-- Supply two dedicated PostgreSQL login roles through -v api_role=... -v worker_role=....
-- Set -v booking_hold=true when the API is configured with a hold offer source.
\if :{?booking_hold}
\else
\set booking_hold false
\endif
\if :{?payment_sandbox}
\else
\set payment_sandbox false
\endif
BEGIN;

SELECT current_database() AS database_name,
       (:'api_role' <> :'worker_role' AND
        (SELECT count(*) = 2
         FROM pg_roles r
         WHERE r.rolname IN (:'api_role', :'worker_role') AND r.rolcanlogin
           AND NOT (r.rolsuper OR r.rolcreaterole OR r.rolcreatedb OR r.rolreplication OR r.rolbypassrls)
           AND r.rolname <> current_user
           AND NOT pg_has_role(r.oid, (SELECT datdba FROM pg_database WHERE datname = current_database()), 'MEMBER'))
       ) AS valid_runtime_roles,
       :'booking_hold'::boolean AS runtime_booking_hold,
       :'payment_sandbox'::boolean AS runtime_payment_sandbox
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
-- Table-level revokes do not remove column grants from a previous deployment.
REVOKE SELECT (created_by, category_id, idempotency_key, request_digest), UPDATE (status),
    INSERT (business_id, event_id, category_id, created_by, status, expires_at, price_revision,
        rent, platform_fee, provider_fee, terms_snapshot, refund_policy_version,
        refund_policy_snapshot, idempotency_key, request_digest)
    ON public.bookings FROM :"api_role";
REVOKE SELECT (block_id, status), UPDATE (status),
    INSERT (booking_id, event_id, event_slot_id, stand_id, block_id, status)
    ON public.allocations FROM :"api_role";
REVOKE INSERT (booking_id, actor_id, from_status, to_status, reason)
    ON public.booking_status_history FROM :"api_role";
REVOKE SELECT (id, booking_id, created_by, provider, status, amount, currency, checkout_url, expires_at, idempotency_key, request_digest),
    INSERT (booking_id, created_by, provider, method, status, amount, expires_at, idempotency_key, request_digest),
    UPDATE (status, provider_token, checkout_url, updated_at, paid_at) ON public.payment_attempts FROM :"api_role";
REVOKE SELECT (id, provider, dedupe_key), INSERT (provider, dedupe_key, provider_status, outcome, payload),
    UPDATE (outcome, attempt_id) ON public.payment_events FROM :"api_role";
REVOKE SELECT (id), INSERT (attempt_id, booking_id, kind, detail) ON public.payment_exceptions FROM :"api_role";
REVOKE SELECT (id), INSERT (kind, ref_type, ref_id) ON public.ledger_transactions FROM :"api_role";
REVOKE SELECT (transaction_id, direction, amount), INSERT (transaction_id, account, direction, amount)
    ON public.ledger_entries FROM :"api_role";

GRANT SELECT ON public.business_categories, public.businesses, public.organizations, public.memberships,
    public.consent_events, public.events, public.event_slots, public.event_status_history,
    public.event_pricing_versions, public.event_revisions, public.event_revision_bookings,
    public.event_revision_decisions, public.event_revision_applications, public.organization_event_history TO :"api_role";
GRANT SELECT (id, email, password_hash, email_verified_at),
    UPDATE (password_hash, email_verified_at) ON public.users TO :"api_role";
GRANT SELECT (token_hash, user_id, purpose, expires_at, used_at, created_at),
    INSERT (token_hash, user_id, purpose, expires_at), UPDATE (used_at)
    ON public.auth_action_tokens TO :"api_role";
GRANT SELECT (id, user_id, expires_at, revoked_at) ON public.sessions TO :"api_role";
GRANT SELECT (token_hash, session_id, used_at) ON public.session_refresh_tokens TO :"api_role";
GRANT SELECT (id, business_id, event_id, status, expires_at, created_at, refund_policy_version,
    price_revision, rent, platform_fee, provider_fee, total) ON public.bookings TO :"api_role";
GRANT SELECT (booking_id, stand_id, event_slot_id) ON public.allocations TO :"api_role";
GRANT SELECT (event_id, status) ON public.allocations TO :"api_role";
GRANT SELECT ON public.event_layouts, public.layout_blocks, public.layout_areas, public.stands,
    public.event_layout_revisions TO :"api_role";
GRANT INSERT (event_id, revision, width, height, cell_meters, updated_by),
    UPDATE (revision, width, height, cell_meters, updated_by, updated_at) ON public.event_layouts TO :"api_role";
GRANT INSERT (id, event_id, label, x, y, width, height),
    UPDATE (label, x, y, width, height), DELETE ON public.layout_blocks TO :"api_role";
GRANT INSERT (event_id, kind, x, y, width, height), DELETE ON public.layout_areas TO :"api_role";
GRANT INSERT (id, event_id, block_id, code, x, y, width, height, rotation, category_ids, facilities, active),
    UPDATE (block_id, code, x, y, width, height, rotation, category_ids, facilities, active, removed_at)
    ON public.stands TO :"api_role";
GRANT INSERT (event_id, revision, actor_id, snapshot) ON public.event_layout_revisions TO :"api_role";
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
GRANT INSERT (event_id, revision, requested_by, reason, snapshot_before, snapshot_after, idempotency_key, request_digest)
    ON public.event_revisions TO :"api_role";
GRANT INSERT (revision_id, event_id, booking_id, status_at_proposal) ON public.event_revision_bookings TO :"api_role";
GRANT INSERT (revision_id, booking_id, decided_by, decision) ON public.event_revision_decisions TO :"api_role";
GRANT INSERT (revision_id, event_id, applied_by, supersedes_through) ON public.event_revision_applications TO :"api_role";
GRANT INSERT (organization_id, reported_by, name, starts_at, ends_at, timezone, city, address, idempotency_key)
    ON public.organization_event_history TO :"api_role";
GRANT EXECUTE ON FUNCTION public.cancel_unpaid_event_bookings(uuid, uuid) TO :"api_role";
-- Produk dan stok usaha (E10): stok hanya berubah bersama baris inventory_movements.
GRANT SELECT ON public.products, public.inventory_movements TO :"api_role";
GRANT INSERT (business_id, name, sku, price, cost, category_id, stock, created_by),
    UPDATE (name, sku, price, cost, category_id, stock, archived_at, updated_at) ON public.products TO :"api_role";
GRANT INSERT (business_id, product_id, delta, reason, note, actor_id, sale_ref, adjustment_ref) ON public.inventory_movements TO :"api_role";
-- Koreksi penjualan dan pengeluaran (E10-05): append-only, tanpa UPDATE atau DELETE untuk role API.
GRANT SELECT ON public.sale_adjustments, public.sale_adjustment_items, public.expenses TO :"api_role";
GRANT INSERT (business_id, sale_ref, type, reason, amount, created_by, idempotency_key, request_digest)
    ON public.sale_adjustments TO :"api_role";
GRANT INSERT (adjustment_id, sale_item_id, quantity) ON public.sale_adjustment_items TO :"api_role";
GRANT INSERT (business_id, event_id, category, amount, occurred_at, note, created_by, idempotency_key, request_digest)
    ON public.expenses TO :"api_role";
-- Kasir (E10): sesi per perangkat dan penjualan hasil sync; riwayat penjualan tidak pernah diubah langsung.
GRANT SELECT ON public.pos_sessions, public.sales, public.sale_items TO :"api_role";
GRANT INSERT (business_id, device_id, event_id, opened_by), UPDATE (closed_at) ON public.pos_sessions TO :"api_role";
GRANT INSERT (business_id, sale_id, device_id, session_id, event_id, client_created_at, schema_version, payload_hash,
    request_digest, currency, method, discount, total, synced_by) ON public.sales TO :"api_role";
GRANT INSERT (business_id, sale_ref, position, product_id, product_name, quantity, unit_price, unit_cost, discount, subtotal)
    ON public.sale_items TO :"api_role";
-- Admin platform (E16): keanggotaan admin hanya dibaca API; pemberian lewat perintah grant-admin.
GRANT SELECT ON public.platform_admins, public.admin_audit, public.event_plan_versions, public.event_plans TO :"api_role";
GRANT INSERT (actor_id, action, resource_type, resource_id, reason, before, after, request_id) ON public.admin_audit TO :"api_role";
GRANT UPDATE (verification_status, verified_at) ON public.organizations TO :"api_role";

\if :runtime_booking_hold
GRANT SELECT ON public.hold_policy_versions TO :"api_role";
GRANT SELECT (created_by, category_id, idempotency_key, request_digest), UPDATE (status),
    INSERT (business_id, event_id, category_id, created_by, status, expires_at, price_revision,
        rent, platform_fee, provider_fee, terms_snapshot, refund_policy_version,
        refund_policy_snapshot, idempotency_key, request_digest)
    ON public.bookings TO :"api_role";
GRANT SELECT (block_id, status), UPDATE (status),
    INSERT (booking_id, event_id, event_slot_id, stand_id, block_id, status)
    ON public.allocations TO :"api_role";
GRANT INSERT (booking_id, actor_id, from_status, to_status, reason)
    ON public.booking_status_history TO :"api_role";
\endif

\if :runtime_payment_sandbox
GRANT SELECT (created_by), UPDATE (status) ON public.bookings TO :"api_role";
GRANT UPDATE (status) ON public.allocations TO :"api_role";
GRANT INSERT (booking_id, from_status, to_status, reason) ON public.booking_status_history TO :"api_role";
GRANT SELECT (id, booking_id, created_by, provider, status, amount, currency, checkout_url, expires_at, idempotency_key, request_digest),
    INSERT (booking_id, created_by, provider, method, status, amount, expires_at, idempotency_key, request_digest),
    UPDATE (status, provider_token, checkout_url, updated_at, paid_at) ON public.payment_attempts TO :"api_role";
GRANT SELECT (id, provider, dedupe_key), INSERT (provider, dedupe_key, provider_status, outcome, payload),
    UPDATE (outcome, attempt_id) ON public.payment_events TO :"api_role";
GRANT SELECT (id), INSERT (attempt_id, booking_id, kind, detail) ON public.payment_exceptions TO :"api_role";
GRANT SELECT (id), INSERT (kind, ref_type, ref_id) ON public.ledger_transactions TO :"api_role";
GRANT SELECT (transaction_id, direction, amount), INSERT (transaction_id, account, direction, amount)
    ON public.ledger_entries TO :"api_role";
\endif

GRANT SELECT (id, status, expires_at, event_id), UPDATE (status) ON public.bookings TO :"worker_role";
GRANT SELECT (booking_id, status), UPDATE (status) ON public.allocations TO :"worker_role";
GRANT SELECT (id), INSERT (booking_id, from_status, to_status, reason)
    ON public.booking_status_history TO :"worker_role";

COMMIT;
