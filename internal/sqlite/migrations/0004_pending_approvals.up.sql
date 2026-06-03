CREATE TABLE pending_approvals (
    id              TEXT    NOT NULL PRIMARY KEY,
    status          TEXT    NOT NULL CHECK (status IN ('pending', 'approved', 'denied', 'expired')),
    action          TEXT    NOT NULL,
    justification   TEXT    NOT NULL,
    requester       TEXT    NOT NULL,
    decision_reason TEXT,
    created_at      INTEGER NOT NULL,
    decided_at      INTEGER,
    expires_at      INTEGER NOT NULL
) STRICT;

CREATE INDEX idx_pending_approvals_status_created ON pending_approvals (status, created_at DESC);
