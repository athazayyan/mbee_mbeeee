-- E03 katalog publik: daftar acara terbit diurutkan dari tanggal mulai terdekat.
CREATE INDEX events_public_catalog_idx ON events (starts_at, id) WHERE status IN ('published', 'live');
CREATE INDEX events_public_city_idx ON events (lower(city)) WHERE status IN ('published', 'live');
