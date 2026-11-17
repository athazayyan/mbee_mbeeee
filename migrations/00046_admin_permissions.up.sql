-- E16-01: fine-grained admin permissions. Existing admins keep every permission until narrowed.
ALTER TABLE platform_admins ADD COLUMN permissions text[] NOT NULL DEFAULT ARRAY[
    'directory.read', 'organizations.verify', 'audit.read', 'payments.read', 'payments.manage',
    'funds.manage', 'pricing.manage', 'moderation.manage'];
ALTER TABLE platform_admins ADD CONSTRAINT platform_admin_permissions_check CHECK (
    cardinality(permissions) >= 1 AND permissions <@ ARRAY[
    'directory.read', 'organizations.verify', 'audit.read', 'payments.read', 'payments.manage',
    'funds.manage', 'pricing.manage', 'moderation.manage']::text[]);

-- E16-02: an exception is resolved by a named admin, with a note.
ALTER TABLE payment_exceptions
    ADD COLUMN resolved_by uuid REFERENCES users (id),
    ADD COLUMN resolved_at timestamptz,
    ADD COLUMN resolution_note text CHECK (resolution_note IS NULL OR char_length(resolution_note) <= 500);

-- E16-03: who published a plan price version.
ALTER TABLE plan_price_versions ADD COLUMN created_by uuid REFERENCES users (id);
