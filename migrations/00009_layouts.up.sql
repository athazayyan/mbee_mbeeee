-- E05 denah grid: satu denah aktif per acara, disimpan sebagai revisi penuh.
CREATE TABLE event_layouts (
    event_id uuid PRIMARY KEY REFERENCES events (id) ON DELETE CASCADE,
    revision integer NOT NULL CHECK (revision >= 1),
    width integer NOT NULL CHECK (width BETWEEN 4 AND 60),
    height integer NOT NULL CHECK (height BETWEEN 4 AND 60),
    cell_meters numeric(4, 2) NOT NULL CHECK (cell_meters BETWEEN 0.5 AND 10),
    updated_by uuid NOT NULL REFERENCES users (id),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE layout_blocks (
    id uuid PRIMARY KEY,
    event_id uuid NOT NULL REFERENCES event_layouts (event_id) ON DELETE CASCADE,
    label text NOT NULL CHECK (char_length(btrim(label)) BETWEEN 1 AND 40),
    x integer NOT NULL CHECK (x >= 0),
    y integer NOT NULL CHECK (y >= 0),
    width integer NOT NULL CHECK (width >= 1),
    height integer NOT NULL CHECK (height >= 1),
    UNIQUE (event_id, id)
);

CREATE TABLE layout_areas (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id uuid NOT NULL REFERENCES event_layouts (event_id) ON DELETE CASCADE,
    kind text NOT NULL CHECK (kind IN ('walkway', 'entrance', 'stage', 'restricted')),
    x integer NOT NULL CHECK (x >= 0),
    y integer NOT NULL CHECK (y >= 0),
    width integer NOT NULL CHECK (width >= 1),
    height integer NOT NULL CHECK (height >= 1)
);

CREATE INDEX layout_areas_event_idx ON layout_areas (event_id);

-- Stand tidak pernah dihapus permanen agar allocations historis tetap merujuk stand yang sah.
CREATE TABLE stands (
    id uuid PRIMARY KEY,
    event_id uuid NOT NULL REFERENCES event_layouts (event_id) ON DELETE CASCADE,
    block_id uuid,
    code text NOT NULL CHECK (char_length(btrim(code)) BETWEEN 1 AND 12),
    x integer NOT NULL CHECK (x >= 0),
    y integer NOT NULL CHECK (y >= 0),
    width integer NOT NULL CHECK (width >= 1),
    height integer NOT NULL CHECK (height >= 1),
    rotation smallint NOT NULL DEFAULT 0 CHECK (rotation IN (0, 90)),
    category_ids uuid[] NOT NULL DEFAULT '{}',
    facilities text[] NOT NULL DEFAULT '{}',
    active boolean NOT NULL DEFAULT true,
    removed_at timestamptz,
    UNIQUE (event_id, id)
);

CREATE UNIQUE INDEX stands_event_code_idx ON stands (event_id, upper(code)) WHERE removed_at IS NULL;
CREATE INDEX stands_event_active_idx ON stands (event_id) WHERE removed_at IS NULL AND active;

CREATE TABLE event_layout_revisions (
    event_id uuid NOT NULL REFERENCES event_layouts (event_id) ON DELETE CASCADE,
    revision integer NOT NULL CHECK (revision >= 1),
    actor_id uuid NOT NULL REFERENCES users (id),
    snapshot jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (event_id, revision)
);

-- Kunci stand yang dijanjikan di 00005. NOT VALID: baris lama tidak diperiksa, baris baru wajib sah.
ALTER TABLE allocations
    ADD CONSTRAINT allocations_stand_fkey FOREIGN KEY (event_id, stand_id) REFERENCES stands (event_id, id) NOT VALID;
