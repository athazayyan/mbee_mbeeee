CREATE TABLE business_categories (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name text NOT NULL UNIQUE
);

INSERT INTO business_categories (name) VALUES
    ('Makanan dan minuman'), ('Fesyen'), ('Kerajinan'), ('Jasa'), ('Lainnya');

CREATE TABLE businesses (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id uuid NOT NULL REFERENCES users (id),
    category_id uuid NOT NULL REFERENCES business_categories (id),
    name text NOT NULL CHECK (char_length(btrim(name)) BETWEEN 1 AND 120),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE organizations (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id uuid NOT NULL REFERENCES users (id),
    name text NOT NULL CHECK (char_length(btrim(name)) BETWEEN 1 AND 120),
    verification_status text NOT NULL DEFAULT 'unverified'
        CHECK (verification_status IN ('unverified', 'verified')),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE memberships (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    business_id uuid REFERENCES businesses (id) ON DELETE CASCADE,
    organization_id uuid REFERENCES organizations (id) ON DELETE CASCADE,
    role text NOT NULL CHECK (role IN ('owner', 'member')),
    CHECK (num_nonnulls(business_id, organization_id) = 1),
    UNIQUE (user_id, business_id),
    UNIQUE (user_id, organization_id)
);

CREATE INDEX memberships_business_idx ON memberships (business_id) WHERE business_id IS NOT NULL;
CREATE INDEX memberships_organization_idx ON memberships (organization_id) WHERE organization_id IS NOT NULL;

CREATE TABLE consent_events (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id uuid NOT NULL REFERENCES businesses (id),
    actor_id uuid NOT NULL REFERENCES users (id),
    purpose text NOT NULL CHECK (purpose = 'analytics'),
    granted boolean NOT NULL,
    policy_version text NOT NULL CHECK (char_length(btrim(policy_version)) BETWEEN 1 AND 80),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX consent_events_business_idx ON consent_events (business_id, created_at DESC, id DESC);
