CREATE TABLE jobs (
    id              TEXT    NOT NULL PRIMARY KEY,
    status          TEXT    NOT NULL CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'cancelled')),
    trigger_kind    TEXT    NOT NULL CHECK (trigger_kind IN ('ephemeral', 'cron', 'long_running')),
    name            TEXT    NOT NULL,
    command         TEXT    NOT NULL,
    image           TEXT    NOT NULL,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    started_at      INTEGER,
    completed_at    INTEGER,
    error_message   TEXT,
    exit_code       INTEGER
) STRICT;

CREATE INDEX idx_jobs_status_created_at ON jobs (status, created_at DESC);
CREATE INDEX idx_jobs_created_at        ON jobs (created_at DESC);
