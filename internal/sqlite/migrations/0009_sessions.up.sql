CREATE TABLE sessions (
    id           TEXT    NOT NULL PRIMARY KEY,
    name         TEXT    NOT NULL UNIQUE,
    image        TEXT    NOT NULL,
    state        TEXT    NOT NULL CHECK (state IN ('running', 'stopped')),
    fork_id      TEXT    NOT NULL,
    fork_path    TEXT    NOT NULL,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    last_used_at INTEGER
) STRICT;

CREATE INDEX idx_sessions_created_at ON sessions (created_at DESC);
