-- Concurrency and rate limits (limits.md). A limit applies to one activity
-- type within a queue; a type without a row is unlimited.
--
-- max_concurrent caps the type's running ('processing') activities across
-- every worker of the queue. It is not a counter: a claim counts the running
-- rows (idx_runnerq_processing_v2) while it holds this row's lock, so a
-- worker that dies leaves nothing to drift.
--
-- rate_tokens and rate_per_seconds are a token bucket: at most rate_tokens
-- claims in a burst, refilled continuously at rate_tokens per
-- rate_per_seconds. tokens and refilled_at are the bucket's level and when it
-- was last brought up to date, by the database clock; a claim updates them
-- under the same lock. Both rate columns are set or both are null.
--
-- updated_at is when the definition (max_concurrent, rate_tokens,
-- rate_per_seconds) last changed.
CREATE TABLE IF NOT EXISTS runnerq_limits (
    queue_name TEXT NOT NULL,
    activity_type TEXT NOT NULL,
    max_concurrent INTEGER,
    rate_tokens INTEGER,
    rate_per_seconds DOUBLE PRECISION,
    tokens DOUBLE PRECISION NOT NULL DEFAULT 0,
    refilled_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (queue_name, activity_type),
    CHECK (max_concurrent IS NULL OR max_concurrent >= 1),
    CHECK ((rate_tokens IS NULL) = (rate_per_seconds IS NULL)),
    CHECK (rate_tokens IS NULL OR (rate_tokens >= 1 AND rate_per_seconds > 0)),
    CHECK (max_concurrent IS NOT NULL OR rate_tokens IS NOT NULL)
);
