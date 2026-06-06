-- Reverse 0010: drop cron definitions and the scheduling columns, restoring the
-- original status set. Scheduled cron jobs cannot exist under the old CHECK, so
-- they are removed.

DELETE FROM jobs WHERE status = 'scheduled';

CREATE TABLE jobs_old (
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
    exit_code       INTEGER,
    credentials     TEXT    NOT NULL DEFAULT ''
) STRICT;

INSERT INTO jobs_old (
    id, status, trigger_kind, name, command, image, created_at, updated_at,
    started_at, completed_at, error_message, exit_code, credentials
)
SELECT
    id, status, trigger_kind, name, command, image, created_at, updated_at,
    started_at, completed_at, error_message, exit_code, credentials
FROM jobs;

DROP TABLE jobs;
ALTER TABLE jobs_old RENAME TO jobs;

CREATE INDEX idx_jobs_status_created_at ON jobs (status, created_at DESC);
CREATE INDEX idx_jobs_created_at        ON jobs (created_at DESC);
