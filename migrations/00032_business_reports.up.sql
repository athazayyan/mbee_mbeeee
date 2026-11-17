-- E14 rapor usaha. Rapor adalah snapshot periode pada saat dibuat; data yang tersinkron belakangan
-- tidak mengubah rapor lama. Tautan berbagi disimpan sebagai hash token dan memiliki masa berlaku.
CREATE TABLE business_reports (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id uuid NOT NULL REFERENCES businesses (id) ON DELETE CASCADE,
    event_id uuid REFERENCES events (id),
    period_start timestamptz NOT NULL,
    period_end timestamptz NOT NULL,
    timezone text NOT NULL,
    status text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'running', 'succeeded', 'failed')),
    error_code text,
    snapshot jsonb,
    -- Kode pendek yang dicetak di rapor agar pembaca dapat mencocokkannya dengan TenantKu.
    verification_code text NOT NULL UNIQUE,
    cutoff_at timestamptz,
    requested_by uuid NOT NULL REFERENCES users (id),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    completed_at timestamptz,
    CHECK (period_end > period_start),
    UNIQUE (business_id, id)
);

CREATE INDEX business_reports_business_idx ON business_reports (business_id, created_at DESC, id DESC);

CREATE TABLE report_shares (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id uuid NOT NULL,
    report_id uuid NOT NULL,
    token_hash bytea NOT NULL UNIQUE,
    scope text NOT NULL DEFAULT 'summary' CHECK (scope IN ('summary')),
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    created_by uuid NOT NULL REFERENCES users (id),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    last_viewed_at timestamptz,
    view_count integer NOT NULL DEFAULT 0,
    FOREIGN KEY (business_id, report_id) REFERENCES business_reports (business_id, id) ON DELETE CASCADE
);

CREATE INDEX report_shares_report_idx ON report_shares (report_id, created_at DESC);
