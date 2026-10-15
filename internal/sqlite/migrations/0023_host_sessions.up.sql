ALTER TABLE sessions ADD COLUMN execution_kind TEXT NOT NULL DEFAULT 'vm' CHECK (execution_kind IN ('vm', 'host'));
ALTER TABLE sessions ADD COLUMN host_user TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN host_uid INTEGER NOT NULL DEFAULT 0 CHECK (host_uid >= 0);
ALTER TABLE sessions ADD COLUMN host_home TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN working_dir TEXT NOT NULL DEFAULT '' CHECK (
    execution_kind = 'vm' OR (
        host_uid > 0 AND length(host_user) > 0
        AND substr(host_home, 1, 1) = '/' AND substr(working_dir, 1, 1) = '/'
        AND image = '' AND fork_id = '' AND fork_path = ''
        AND run_app = 0 AND volume_id IS NULL AND env_vars = '[]'
        AND prev_fork_id IS NULL AND prev_fork_path IS NULL
    )
);
