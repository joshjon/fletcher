-- Persistent volumes (Milestone 12): first-class disks with their own
-- lifecycle, attached to sessions as a second drive. A session's fork dies
-- with redeploy/delete; its volume does not - that is the point.
CREATE TABLE volumes (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    -- path is the host path of the backing ext4 image.
    path TEXT NOT NULL,
    -- size_bytes is the provisioned capacity (the backing file is sparse, so
    -- real disk use grows with data, capped here).
    size_bytes INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
) STRICT;

-- volume_id attaches a volume to at most one session (enforced in the
-- manager); detaching is clearing it or deleting the session. The volume row
-- and its backing disk outlive the session.
ALTER TABLE sessions ADD COLUMN volume_id TEXT REFERENCES volumes (id);
