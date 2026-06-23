-- User-set environment variables injected into a session's app/process at boot.
-- Stored as a JSON array of {"name","value","secret_name"} objects, mirroring
-- the one-row read pattern of jobs.credentials; empty string means none. A
-- secret var carries secret_name (a SecretService key) and an empty value - its
-- value is resolved from the secret store at boot and never stored here.
ALTER TABLE sessions ADD COLUMN env_vars TEXT NOT NULL DEFAULT '';
