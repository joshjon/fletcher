-- Reports (Milestone 14): structured results an agent posts via the
-- fletcher report MCP tool ("web app ready", "scrape finished"). The surviving
-- half of the inbox idea: a report becomes push-notification content and stays
-- queryable; the feed UI is parked until a usage signal.
CREATE TABLE reports (
    id TEXT PRIMARY KEY,
    -- source_type/source_id/source_name tie a report to what produced it
    -- ("session" | "job"; ids/names may be empty when unattributed).
    source_type TEXT NOT NULL,
    source_id TEXT NOT NULL,
    source_name TEXT NOT NULL,
    title TEXT NOT NULL,
    summary TEXT NOT NULL,
    -- status is the report's tone: "info" | "success" | "warning" | "error".
    status TEXT NOT NULL CHECK (status IN ('info', 'success', 'warning', 'error')),
    -- link is an optional URL the report points at (e.g. a published port).
    link TEXT NOT NULL,
    created_at INTEGER NOT NULL
) STRICT;
