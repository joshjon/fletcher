-- Runtime-mutable operational settings, edited via `fletcher settings`.
-- Applied at daemon startup over the flag/env config; changes take effect on
-- the next restart. Bootstrap config (database, socket, age key, listen
-- addresses) stays flag/env only and is not stored here.
CREATE TABLE settings (
    key        TEXT    PRIMARY KEY,
    value      TEXT    NOT NULL,
    updated_at INTEGER NOT NULL
) STRICT;
