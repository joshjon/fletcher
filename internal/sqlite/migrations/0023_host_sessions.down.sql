-- Host registrations cannot be represented by the older VM-only schema.
-- This removes metadata only; no host filesystem operation is performed.
DELETE FROM sessions WHERE execution_kind = 'host';
ALTER TABLE sessions DROP COLUMN working_dir;
ALTER TABLE sessions DROP COLUMN host_home;
ALTER TABLE sessions DROP COLUMN host_uid;
ALTER TABLE sessions DROP COLUMN host_user;
ALTER TABLE sessions DROP COLUMN execution_kind;
