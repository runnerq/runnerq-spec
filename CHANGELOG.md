# Changelog

## 0.8.0

- Conductor protocol: `activity.notices`, an agent event announcing the
  lifecycle changes no event is stored for (`activity.created`,
  `activity.scheduled`, `attempt.started`, `attempt.succeeded`), sent only
  while `SessionConfig.notices` is on. Best effort: no cursor, never stored.

## 0.7.0

Leaner storage: about half the writes and storage per activity, measured on
Postgres 17 (5,000 activities: WAL for submit, claim and complete 5.3 KB →
2.5 KB, a lease renewal 856 B → 173 B, on disk 2.6 KB → 1.3 KB). Breaking,
before any release; `0001_baseline` is edited this once.

- Events store only what the activity row can't say. `Enqueued`,
  `Scheduled`, `Dequeued`, `Completed`, `ResultStored` and `LeaseExtended`
  are gone (the row's times and `runnerq_results` say them); an event ending
  an attempt records the attempt's `started_at`. The storage protocol drops
  `ExtendLease`.
- `RetentionPolicy.Events` (`events_s` in conformance): events of finished
  activities can be trimmed sooner than their tree.
- Indexes: `runnerq_activities` goes from 15 to 9. Seven retired
  (`retired.json` now lists indexes too, dropped after the concurrent
  builds); `idx_runnerq_processing` leaves the lease out so renewals are HOT
  (with `fillfactor = 85`); `idx_runnerq_root_children` leaves roots out;
  `idx_runnerq_root_terminal` covers cancelled roots, which retention needs;
  `idx_runnerq_query_status` leaves completed activities out;
  `idx_runnerq_results_by_owner` serves step lists without the queue.
- `schema/postgres/README.md`: column contracts every implementation keeps.

- 13 more scenarios: the other idempotency policies, shared and reaped
  results waking waiters, park and signal edge cases, the reaper's limit,
  retention with live work and separate ages, and Go and TypeScript
  claiming one queue concurrently (`submit_many` and `drain` steps).

- `schema/postgres/events.schema.json`: the detail every implementation
  writes for each stored event type (`runnerq_events.detail`), so an event
  reads the same whichever SDK wrote it. The conformance runner checks every
  event a scenario writes against it.

## 0.6.0

Phase 5, first part: cross-language conformance.

- `conformance/`: the driver protocol each SDK implements, and 29 scenarios
  (claims, acks, failures, leases, durable waits, idempotency, retention, and
  two mixed Go/TypeScript fleets) checked against the database by the
  runner, `tools/conformance`.
- Decided by the scenarios: a terminal outcome (dead letter) doesn't count
  another attempt, so `retry_count + 1` is always the last attempt; the
  reaper records the expired claim as `last_worker_id`; a batch claim returns
  claims in claim order; an absent payload is stored as JSON null.

## 0.5.0

Phase 4b: the conductor protocol. Breaking, before any release.

- `protocol/conductor/conductor.schema.json`: every message (`x-messages`)
  and type, with an example of each and the protocol document (README.md),
  moved from runnerq-cloud. Decisions where the copies disagreed:
  - One executor shape everywhere: `ExecutorState` with `claim_lag_ms`,
    `heartbeat_failures` and `counters` always present, counters as
    non-negative integers; the hosted report is `ExecutorReport`, replacing
    the storage protocol's `executor_report.schema.json`.
  - A request type per command (`CancelRequest`, …) instead of one generic
    command; concrete `ActivityPage`, `StepPage`, `EventPage`.
  - `events.unsubscribe` takes `{subscription_id}` and answers `{}`.
  - The reserved `retention.apply`, `executor.drain` and `executor.resume`
    are dropped until they are designed.
  - Typed enums: statuses, filter operators, error codes, outcomes, cascades,
    wait and step kinds.
- `tools/gen -part conductor` (Go, TypeScript, and TypeScript decode specs);
  `-part executor-report` now emits the conductor report's types.
- `schemacheck`: the schema validator as a package, so implementations can
  check what they send.

## 0.4.0

Phase 4a: the storage protocol.

- `protocol/storage/storage.schema.json`: every storage operation's arguments
  and result, and the types they carry, with the data plane's bounds as JSON
  Schema constraints. `executor_report.schema.json`: worker heartbeats.
- One example per operation, and one report, encoded by the Go adapter;
  `verify` checks them against the schemas.
- `tools/gen -part storage` (Go wire types, Go client, Go server dispatch with
  validation, TypeScript types) and `-part executor-report`. They replace the
  adapter's `tools/generate.py`.

## 0.3.0

Phase 3: the Postgres schema.

- `schema/postgres`: `migrations/0001_baseline.sql` (the Go SDK's schema),
  `concurrent_indexes.json`, `retired.json`, and `catalog.json`, which
  `tools/catalog` reads from Postgres. CI checks it on Postgres 16, 17 and 18.
- `schema/postgres/README.md`: how implementations migrate and validate.
- Vectors: `index_definition`, `column_default` (catalog normalization).
- `tools/gen -part schema`: migrations, concurrent indexes and the catalog as
  Go or TypeScript source.

## 0.2.0

Phase 2: the failure path and the query mappings.

- Vectors: `attempts_remain` (the retry decision), `retry_delay` (backoff),
  `canonical_status`, `canonical_event`, `internal_events`.
- Constant `default_max_retry_delay_seconds` (3600), and the `int` constant
  type: a plain number in TypeScript.

## 0.1.0

First release (phase 1).

- `constants/constants.json`: schema advisory lock key, notification channel
  prefixes, business and step key prefixes, serialization ids, storage error
  kinds.
- Vectors: `checkpoint_id`, `business_key`, `application_key`, `step_key`,
  `serialization/plain_json`.
- `tools/gen`: Go and TypeScript constants.
- `verify`: reference checks for every vector.
