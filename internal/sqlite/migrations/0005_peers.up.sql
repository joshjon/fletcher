CREATE TABLE peers (
    id          TEXT    NOT NULL PRIMARY KEY,
    name        TEXT    NOT NULL UNIQUE,
    public_key  TEXT    NOT NULL,
    allowed_ips TEXT    NOT NULL,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
) STRICT;

CREATE INDEX idx_peers_created_at ON peers (created_at DESC);
