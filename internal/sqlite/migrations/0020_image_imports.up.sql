CREATE TABLE image_imports (
    id TEXT PRIMARY KEY,
    ref TEXT NOT NULL,
    name TEXT NOT NULL,
    is_replacement INTEGER NOT NULL CHECK (is_replacement IN (0, 1)),
    is_update INTEGER NOT NULL DEFAULT 0 CHECK (is_update IN (0, 1)),
    state TEXT NOT NULL CHECK (state IN ('running', 'succeeded', 'failed')),
    error TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
) STRICT;
