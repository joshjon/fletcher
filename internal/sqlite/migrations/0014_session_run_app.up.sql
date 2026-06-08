-- run_app (Milestone 9): when 1, the session boots and runs the image's own app
-- (its captured entrypoint) instead of waiting for exec/shell. Persisted so a
-- restart/wake re-runs the app rather than coming up bare. Default 0 keeps
-- existing sessions as plain interactive environments.
ALTER TABLE sessions ADD COLUMN run_app INTEGER NOT NULL DEFAULT 0 CHECK (run_app IN (0, 1));
