-- Version pinning (versions.md). code_version is the code version an
-- activity must run on: a root gets the enqueuing process's version, a child
-- its root's, at insert. NULL is unstamped work, which every worker claims.
-- Only a repin command changes it afterwards; the reaper, retries and wakes
-- leave it as it is.
--
-- No index: a pinned claim filters on it during the dequeue index walk, as a
-- multi-type claim filters on activity_type, so it stays out of every index
-- key and predicate and a repin is a HOT update.
ALTER TABLE runnerq_activities ADD COLUMN IF NOT EXISTS code_version TEXT;
