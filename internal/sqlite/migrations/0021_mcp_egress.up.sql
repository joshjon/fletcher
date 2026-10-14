CREATE TABLE mcp_egress_operations (
    caller_id TEXT NOT NULL,
    operation_key TEXT NOT NULL,
    request_hash TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('pending', 'executing', 'succeeded', 'failed', 'unknown')),
    approval_id TEXT REFERENCES pending_approvals (id),
    result TEXT NOT NULL DEFAULT '',
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (caller_id, operation_key)
) STRICT;
