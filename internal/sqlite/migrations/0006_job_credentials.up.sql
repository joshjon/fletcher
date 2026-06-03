-- Trusted-credential mode (DESIGN.md §5 + Phase 12). The column stores
-- a JSON array of credential names (e.g. ["claude","codex"]); empty
-- string means no credentials are mounted. Storing JSON instead of a
-- separate join table keeps the supervisor's read path one row, which
-- is the only access pattern.
ALTER TABLE jobs ADD COLUMN credentials TEXT NOT NULL DEFAULT '';
