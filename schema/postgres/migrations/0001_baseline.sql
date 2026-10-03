-- Activities table (permanent - no TTL)
CREATE TABLE IF NOT EXISTS runnerq_activities (
    id UUID PRIMARY KEY,
    queue_name TEXT NOT NULL,
    activity_type TEXT NOT NULL,
    priority INTEGER NOT NULL DEFAULT 1,
    status TEXT NOT NULL DEFAULT 'pending',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    scheduled_at TIMESTAMPTZ,
    started_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    lease_deadline_ms BIGINT,
    current_worker_id TEXT,
    last_worker_id TEXT,
    retry_count INTEGER NOT NULL DEFAULT 0,
    max_retries INTEGER NOT NULL DEFAULT 0,
    timeout_seconds BIGINT NOT NULL DEFAULT 300,
    retry_delay_seconds BIGINT NOT NULL DEFAULT 60,
    max_retry_delay_seconds BIGINT NOT NULL DEFAULT 0,
    last_error TEXT,
    last_error_at TIMESTAMPTZ,
    metadata JSONB,
    idempotency_key TEXT
);

-- Inputs, apart from the activity rows that claims, leases and status
-- changes rewrite: an activity's payload is written once and read when it is
-- claimed or inspected. serialization names the payload's encoding
-- ('json-v1': plain JSON, the only one Go writes; the TypeScript SDK also
-- writes 'superjson-v1').
CREATE TABLE IF NOT EXISTS runnerq_inputs (
    activity_id UUID PRIMARY KEY,
    queue_name TEXT NOT NULL,
    payload JSONB NOT NULL,
    serialization TEXT NOT NULL DEFAULT 'json-v1'
);

-- Migration: payloads used to live in runnerq_activities.payload. Move them
-- into runnerq_inputs and drop the column, in the schema transaction, so a
-- database is either wholly before or wholly after the move. Processes on
-- an older version of this SDK can't use the database afterwards: stop them
-- before starting this one.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.columns
               WHERE table_schema = current_schema()
                 AND table_name = 'runnerq_activities' AND column_name = 'payload') THEN
        INSERT INTO runnerq_inputs (activity_id, queue_name, payload)
            SELECT id, queue_name, payload FROM runnerq_activities
            ON CONFLICT (activity_id) DO NOTHING;
        ALTER TABLE runnerq_activities DROP COLUMN payload;
        ALTER TABLE runnerq_activities ALTER COLUMN max_retries SET DEFAULT 0;
    END IF;
END $$;

-- Indexes for efficient queries
--
-- NOTE: indexes on runnerq_activities and runnerq_results are NOT created
-- here: see concurrent_indexes.json. A plain CREATE INDEX takes a SHARE lock
-- that blocks all writes on the table for the duration of the build; at boot,
-- on a hot queue, that stalls every enqueue/dequeue/ack in the cluster.
--
-- Every index on runnerq_activities is written again by each status change,
-- so an index must earn its place. Room left on each page lets lease renewals,
-- which change no indexed column, update in place (HOT).
ALTER TABLE runnerq_activities SET (fillfactor = 85);

-- Migration: add column for existing deployments (idempotent, metadata-only on PG 11+).
ALTER TABLE runnerq_activities
    ADD COLUMN IF NOT EXISTS max_retry_delay_seconds BIGINT NOT NULL DEFAULT 0;

-- Lineage columns for parent/child activity tracking.
ALTER TABLE runnerq_activities
    ADD COLUMN IF NOT EXISTS parent_activity_id UUID,
    ADD COLUMN IF NOT EXISTS root_activity_id UUID,
    ADD COLUMN IF NOT EXISTS depth SMALLINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS waiting_result_id UUID;

CREATE INDEX IF NOT EXISTS idx_runnerq_parent_id
    ON runnerq_activities(parent_activity_id)
    WHERE parent_activity_id IS NOT NULL;

-- Idempotency keys table
CREATE TABLE IF NOT EXISTS runnerq_idempotency (
    queue_name TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    activity_id UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (queue_name, idempotency_key)
);

-- Events: what the activity row can't say (events.schema.json). Retention
-- removes them with their tree, or sooner with an events window.
CREATE TABLE IF NOT EXISTS runnerq_events (
    id BIGSERIAL PRIMARY KEY,
    activity_id UUID NOT NULL,
    queue_name TEXT NOT NULL,
    event_type TEXT NOT NULL,
    worker_id TEXT,
    detail JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_runnerq_events_activity
    ON runnerq_events(activity_id, created_at DESC);
-- Per-queue log order: the live stream's tail (id > cursor) and the events
-- retention walk.
CREATE INDEX IF NOT EXISTS idx_runnerq_events_queue_seq
    ON runnerq_events(queue_name, id);

-- Results table. Rows live until the retention sweeper deletes their
-- workflow tree (or forever when retention is not configured).
CREATE TABLE IF NOT EXISTS runnerq_results (
    activity_id UUID PRIMARY KEY,
    queue_name TEXT NOT NULL,
    state TEXT NOT NULL,
    data JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- owner_activity_id ties every result row to the activity whose lifetime
-- governs it. For normal activity results it equals activity_id; for Run/
-- Sleep checkpoints (whose activity_id is a synthetic UUIDv5 matching no
-- activity row) it is the handler's activity — without it, checkpoint rows
-- could never be garbage-collected. NULL on legacy rows (never swept).
ALTER TABLE runnerq_results
    ADD COLUMN IF NOT EXISTS owner_activity_id UUID;

-- step is the human identity of a checkpoint row, "kind:name" (e.g.
-- "run:create-transfer", "sleep:retry-backoff") — the same string the checkpoint
-- ID is derived from, persisted so the console can show a workflow's durable
-- step history (the checkpoint ID itself is a one-way hash). NULL for an
-- activity's own final result and for legacy rows. No dedicated index: read only
-- by owner (idx_runnerq_results_by_owner) on the console path, never on a hot path.
ALTER TABLE runnerq_results
    ADD COLUMN IF NOT EXISTS step TEXT;

-- serialization names the result data's encoding, as for inputs.
ALTER TABLE runnerq_results
    ADD COLUMN IF NOT EXISTS serialization TEXT NOT NULL DEFAULT 'json-v1';

-- Worker pools: one row per live engine instance. Heartbeats let us tell
-- which pools are still alive for cluster-wide capacity reporting.
CREATE TABLE IF NOT EXISTS runnerq_worker_pools (
    pool_id        UUID PRIMARY KEY,
    queue_name     TEXT NOT NULL,
    max_workers    INTEGER NOT NULL,
    activity_types TEXT[],
    started_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_runnerq_worker_pools_queue_alive
    ON runnerq_worker_pools(queue_name, last_seen_at);

-- Command ledger: applied RunnerQ Cloud commands and their results, so a
-- repeated command_id replays its result instead of applying twice. Rows are
-- pruned after a week (commands must be replayable for at least 24 hours).
CREATE TABLE IF NOT EXISTS runnerq_commands (
    queue_name  TEXT NOT NULL,
    command_id  TEXT NOT NULL,
    fingerprint TEXT NOT NULL,
    kind        TEXT NOT NULL,
    result      JSONB NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (queue_name, command_id)
);
CREATE INDEX IF NOT EXISTS idx_runnerq_commands_created
    ON runnerq_commands(created_at);

-- Durable references survive handler replay and are removed with consumer trees.
CREATE TABLE IF NOT EXISTS runnerq_dependencies (
 queue_name TEXT NOT NULL,
 waiter_activity_id UUID NOT NULL,
 result_id UUID NOT NULL,
 producer_activity_id UUID,
 created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 PRIMARY KEY (queue_name, waiter_activity_id, result_id)
);
CREATE INDEX IF NOT EXISTS idx_runnerq_dependencies_result
 ON runnerq_dependencies(queue_name, result_id);
