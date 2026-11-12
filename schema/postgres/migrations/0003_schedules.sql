-- Cron schedules (schedules.md). A schedule belongs to a queue and is named
-- within it. Workers fire due schedules: in one transaction each claims the
-- row (FOR UPDATE SKIP LOCKED), enqueues an activity per fire time with the
-- idempotency key schedule:<name>:<fire time>, and advances next_fire_at, so
-- a tick fires once however many workers race for it.
--
-- payload and serialization are the fired activity's input, as in
-- runnerq_inputs. options holds the fired activity's execution options
-- (priority, max_retries, timeout_seconds, retry_delay_seconds,
-- max_retry_delay_seconds, metadata); an absent key takes the default
-- schedules.md gives. catch_up is 'skip', 'one' or 'all'; catch_up_limit
-- caps how many due ticks 'all' fires at once.
CREATE TABLE IF NOT EXISTS runnerq_schedules (
    queue_name TEXT NOT NULL,
    name TEXT NOT NULL,
    cron TEXT NOT NULL,
    timezone TEXT NOT NULL DEFAULT 'UTC',
    activity_type TEXT NOT NULL,
    payload JSONB NOT NULL DEFAULT 'null'::jsonb,
    serialization TEXT NOT NULL DEFAULT 'json-v1',
    options JSONB NOT NULL DEFAULT '{}'::jsonb,
    catch_up TEXT NOT NULL DEFAULT 'one',
    catch_up_limit INTEGER NOT NULL DEFAULT 100,
    paused BOOLEAN NOT NULL DEFAULT FALSE,
    next_fire_at TIMESTAMPTZ NOT NULL,
    last_fire_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (queue_name, name)
);

-- The due scan: WHERE queue_name = $1 AND NOT paused AND next_fire_at <= NOW()
-- ORDER BY next_fire_at. Paused schedules are left out.
CREATE INDEX IF NOT EXISTS idx_runnerq_schedules_due
    ON runnerq_schedules (queue_name, next_fire_at)
    WHERE NOT paused;
