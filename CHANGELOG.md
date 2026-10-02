# Changelog

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
