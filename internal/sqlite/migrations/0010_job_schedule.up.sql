-- Cron scheduling (DESIGN.md §4: 'cron' is a value of the trigger field, run by
-- the one job engine, not a separate subsystem). A cron job is a definition
-- that rests in the new 'scheduled' status; the supervisor fires it when
-- next_run_at is due by creating a child ephemeral run (parent_id links it back),
-- so each run is a normal job with full history and the runner needs no
-- special-casing. schedule holds the cron expression; next_run_at is the next
-- fire time. parent_id is set on the spawned runs.
--
-- Adding 'scheduled' to the status CHECK requires recreating the table (SQLite
-- cannot ALTER a CHECK constraint). The jobs table has no inbound foreign keys,
-- so a plain recreate is safe.

CREATE TABLE jobs_new (
    id              TEXT    NOT NULL PRIMARY KEY,
    status          TEXT    NOT NULL CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'cancelled', 'scheduled')),
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
    credentials     TEXT    NOT NULL DEFAULT '',
    schedule        TEXT    NOT NULL DEFAULT '',
    next_run_at     INTEGER,
    parent_id       TEXT
) STRICT;

INSERT INTO jobs_new (
    id, status, trigger_kind, name, command, image, created_at, updated_at,
    started_at, completed_at, error_message, exit_code, credentials,
    schedule, next_run_at, parent_id
)
SELECT
    id, status, trigger_kind, name, command, image, created_at, updated_at,
    started_at, completed_at, error_message, exit_code, credentials,
    '', NULL, NULL
FROM jobs;

DROP TABLE jobs;
ALTER TABLE jobs_new RENAME TO jobs;

CREATE INDEX idx_jobs_status_created_at ON jobs (status, created_at DESC);
CREATE INDEX idx_jobs_created_at        ON jobs (created_at DESC);
CREATE INDEX idx_jobs_parent_id         ON jobs (parent_id);
