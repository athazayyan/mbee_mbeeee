-- Ulasan (E15). Satu ulasan per pihak per booking: tenant mengulas penyelenggara, penyelenggara mengulas tenant.
-- Hanya booking confirmed yang acaranya sudah berakhir yang memenuhi syarat; aturan ini diperiksa API.
CREATE TABLE reviews (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    booking_id uuid NOT NULL REFERENCES bookings (id),
    event_id uuid NOT NULL REFERENCES events (id),
    business_id uuid NOT NULL REFERENCES businesses (id),
    organization_id uuid NOT NULL REFERENCES organizations (id),
    author_side text NOT NULL CHECK (author_side IN ('business', 'organization')),
    author_id uuid NOT NULL REFERENCES users (id),
    rating smallint NOT NULL CHECK (rating BETWEEN 1 AND 5),
    comment text CHECK (comment IS NULL OR char_length(btrim(comment)) BETWEEN 1 AND 2000),
    status text NOT NULL DEFAULT 'published' CHECK (status IN ('published', 'hidden')),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    edit_count integer NOT NULL DEFAULT 0 CHECK (edit_count >= 0),
    UNIQUE (booking_id, author_side)
);

CREATE INDEX reviews_organization_public_idx ON reviews (organization_id, created_at DESC, id DESC)
    WHERE author_side = 'business' AND status = 'published';
CREATE INDEX reviews_recent_idx ON reviews (created_at DESC, id DESC);

-- Riwayat edit: versi sebelum setiap perubahan disimpan dan tidak pernah diubah.
CREATE TABLE review_revisions (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    review_id uuid NOT NULL REFERENCES reviews (id),
    rating smallint NOT NULL,
    comment text,
    edited_by uuid NOT NULL REFERENCES users (id),
    replaced_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX review_revisions_review_idx ON review_revisions (review_id, id DESC);
