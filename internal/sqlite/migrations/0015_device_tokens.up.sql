-- device_tokens holds the APNs device tokens registered by paired clients (the
-- iOS app), so the daemon can push approval notifications. The token is the
-- primary key; re-registering the same token is idempotent.
CREATE TABLE device_tokens (
    token      TEXT    NOT NULL PRIMARY KEY,
    created_at INTEGER NOT NULL
) STRICT;
