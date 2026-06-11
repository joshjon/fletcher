-- prev_fork_* (Milestone 11): the fork a redeploy retired, kept on disk so the
-- operator can roll a bad redeploy back in one step (reflink-shared, so it is
-- nearly free). NULL until the session's first redeploy; replaced on each
-- subsequent one; swapped with the active fork on rollback.
ALTER TABLE sessions ADD COLUMN prev_fork_id TEXT;
ALTER TABLE sessions ADD COLUMN prev_fork_path TEXT;
