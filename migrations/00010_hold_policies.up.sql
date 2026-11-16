CREATE TABLE hold_policy_versions (
    version text PRIMARY KEY CHECK (char_length(version) BETWEEN 1 AND 80 AND version = btrim(version)
        AND version !~ '[[:cntrl:]]'),
    effective_at timestamptz NOT NULL UNIQUE,
    platform_fee bigint NOT NULL CHECK (platform_fee BETWEEN 0 AND 9007199254740991),
    provider_fee bigint NOT NULL CHECK (provider_fee BETWEEN 0 AND 9007199254740991),
    refund_terms text NOT NULL CHECK (char_length(btrim(refund_terms)) BETWEEN 1 AND 6000),
    refund_policy jsonb NOT NULL CHECK (jsonb_typeof(refund_policy) = 'object' AND refund_policy <> '{}'::jsonb),
    CHECK (platform_fee + provider_fee <= 9007199254740991)
);
