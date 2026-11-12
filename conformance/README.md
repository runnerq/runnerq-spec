# Conformance

Scenarios every implementation's Postgres storage must pass, so SDKs in
different languages behave the same against one database.

| Path | What it is |
| --- | --- |
| `scenarios/<area>/<name>.json` | A scenario: operations, fixtures and expectations. |
| `../tools/conformance` | The runner: drives an SDK's driver through each scenario and checks the expectations against the database itself. |

An SDK provides a **driver**: a process that performs storage operations on
its own Postgres backend. The runner owns the scenario (ids, ordering,
fixtures) and every expectation, so the checks are written once, in SQL,
whatever the language. With two drivers, steps marked `"by"` run on one or the
other against the same queue: a mixed Go/TypeScript fleet.

```sh
cd tools/conformance
go run . -dsn "$RUNNERQ_TEST_DSN" -driver go="<command>"   # -spec <root> when run elsewhere
go run . -dsn "$RUNNERQ_TEST_DSN" -driver go="…" -driver ts="node …/conformance-driver.mjs"
```

## Driver protocol

The runner starts the driver once and exchanges one JSON object per line:
a request on the driver's stdin, its reply on stdout. Anything the driver
writes to stderr is shown with failures.

```json
{"op": "submit", "args": {…}}
{"ok": {…}}
{"error": {"kind": "claim_lost", "message": "…"}}
```

`kind` is a storage error kind's name (`constants.json`
`storage_error_kind`). Every scenario starts with `open`, giving the driver a
fresh queue; ids are UUIDs the runner chooses.

| Op | Args | Reply |
| --- | --- | --- |
| `open` | `dsn`, `queue` | `{}`. Initialize the schema if needed and use this queue until the next `open`. |
| `submit` | `id`, `type`, `payload`, `priority` (1-4), `max_attempts` (0 unlimited), `timeout_s`, `delay_s`, `metadata`, `parent` (id), `root` (id), `depth`, `key` (`{key, on_duplicate}`: `return_existing`, `no_reuse`, `allow_reuse`, `allow_reuse_on_failure`), `fence` (a claim), `version` (the code version to stamp) | `{}`, or `{"existing": id}` when a returned-existing key matched |
| `claim` | `types`, `limit`, `lease_ms`, `versions` (claim as a worker serving these code versions; absent or empty: an unversioned worker) | `{"claims": [{"id", "token"}]}` in claim order |
| `renew` | `claim`, `lease_ms` | `{"renewed": bool}` |
| `complete` | `claim`, `value` | `{}` |
| `fail` | `claim`, `reason`, `retryable` | `{"outcome": "retrying" \| "failed" \| "dead_letter"}` |
| `checkpoint` | `claim`, `result_id`, `state` (`ok` \| `error`), `data`, `step` | `{}` |
| `register_dependency` | `claim`, `producer` | `{}` |
| `park` | `claim`, `kind` (`sleep` \| `signal` \| `await`), `step`, `wake_at`, `result_id`, `producer` | `{}` |
| `signal` | `target`, `name`, `payload` | `{}` |
| `lookup_key` | `key` (an encoded business key) | `{"id": id}` |
| `reap` | `limit` | `{"count": n}` |
| `cleanup` | `completed_s`, `failed_s`, `events_s` (0 off), `batch` | `{"count": n}`, the roots deleted |
| `get_result` | `id` | `{"result": null \| {"state", "data", "serialization"}}` |
| `upsert_schedule` | `name`, `cron`, `timezone` (default `UTC`), `type`, `payload`, `priority`, `max_attempts`, `timeout_s`, `metadata`, `catch_up` (default `one`), `catch_up_limit` (default 100), `version` (stored as `options.code_version`) | `{}` |
| `pause_schedule`, `resume_schedule`, `delete_schedule` | `name` | `{}` |
| `trigger_schedule` | `name` | `{"id": id}`, the activity enqueued |
| `list_schedules` | | `{"names": [...]}`, sorted |
| `fire_schedules` | | `{"count": n}`, the activities enqueued; fires the queue's due schedules ([schedules.md](../schedules.md)) |
| `command` | `kind` (`cancel`, `retry`, `run_now`, `reschedule`, `set_priority`, `delete`, `signal`, `repin`), `command_id`, `fingerprint`, a target (`ids`, or `filter` and `max`), `dry_run`, `reason`, `cascade`, `reset_attempts`, `at`, `priority`, `name`, `payload` (signal), `version` (repin) | `{"matched", "applied", "cascaded", "more", "replayed", "items": [{"id", "outcome", "status", "error"}]}` |

A claim is `{"id", "token"}` as the driver returned it; drivers keep no state
between requests. A `command` is the storage command API (`ApplyCommand` in
Go, `applyCommand` in TypeScript) with the arguments as given: `command_id`
absent or empty disables the ledger, `fingerprint` absent means the driver
computes it from the other arguments (`vectors/command_fingerprint.json`, as
the SDK clients do), `cascade` and `reset_attempts` default to false, and `filter` is a query filter (`{"field", "op", "value"}`, `and`,
`or`, `not`). In its reply, an item's `outcome` is `applied`, `skipped` or
`would_apply`; `status` is the canonical status after the command (absent once
deleted); `error` is `not_found` or `conflict` for a skipped item, else absent.
Values (`payload`, `value`, `data`) are plain JSON
(`json-v1`). Absent args take these defaults: `priority` 2, `max_attempts` 3,
`timeout_s` 30, `delay_s` 0, `limit` 1, `lease_ms` 30000. The retry delay base
is 1 second. A signal's result id is `checkpoint_id(target, "signal", name)`.

## Scenarios

```json
{
  "description": "What the scenario shows.",
  "steps": [
    {"op": "submit", "as": "a", "type": "t"},
    {"op": "claim", "types": ["t"], "as": ["c"], "expect": {"claims": ["a"]}},
    {"expect_row": {"activity": "a", "status": "processing", "current_worker_id": {"$token": "c"}}}
  ]
}
```

A step is one of:

- **An operation** (`op`, with its args inline). `as` names what it creates:
  the id for `submit` and `trigger_schedule`, the claims for `claim`.
  Arguments refer to names: activity ids by name, claims by name; `ids` is a
  list of names. `wake_in_s` and `at_in_s` become `wake_at` and `at`, that
  many seconds from now. `by` picks the driver when there are several (`go`,
  `ts`; default the first). `expect` checks the reply: `{"error": kind}`
  (`"any"` for any kind), `{"claims": [names]}` (in order),
  `{"items": {name: {field: value}}}` (a command's items, one per name and no
  others), or any other reply field (`outcome`, `count`, `renewed`,
  `existing`, `result`, `applied`, `replayed`, ...).
- **A fixture** the runner applies in SQL:
  - `{"expire_lease": name}` puts the lease 10 seconds in the past.
  - `{"make_due": name}` sets `scheduled_at` to now.
  - `{"backdate": name, "column": "completed_at", "seconds": n}`.
  - `{"backdate_events": name, "seconds": n}` ages the activity's events.
  - `{"sleep_ms": n}`.
  - `{"make_schedule_due": name, "trunc": unit, "back": interval}` sets the
    schedule's `next_fire_at` to `date_trunc(unit, now in UTC) - interval`.
  - `{"fail_writes": table, "on": bool}` makes every insert and update of
    the table in the scenario's queue raise an error, or stops it.
- **A bulk step** the runner performs through drivers:
  - `{"submit_many": {"prefix", "n", "type", "by"}}` submits `n` activities
    named `<prefix>0` to `<prefix>n-1`.
  - `{"each_status": {"prefix", "by"}}` puts one activity in every stored
    status: `<prefix>pending`, `scheduled` (delayed an hour), `retrying`,
    `running` (claimed, as claim `<prefix>running`), `waiting` (parked on a
    sleep for an hour), `completed`, `failed`, `dead_letter` and `cancelled`,
    each of its own type, its name.
  - `{"drain": {"types", "by": [drivers], "limit", "prefix", "n"}}` claims
    from every listed driver at once until the queue is empty, and checks
    each of the `n` activities was claimed exactly once.
  - `{"fire_race": {"by": [drivers], "rounds": n}}` has every listed driver
    call `fire_schedules` `n` times, all at once.
- **An expectation** the runner checks in SQL:
  - `expect_row`: `runnerq_activities` columns of an activity.
  - `expect_events`: the activity's `runnerq_events` types, in insertion order.
  - `expect_result`: a `runnerq_results` row.
  - `expect_absent`: the activity and everything it owns are gone.
  - `expect_schedule`: `runnerq_schedules` columns of a schedule (`name`),
    or `"absent": true`.
  - `expect_fires`: `{"schedule", "type", "count", "trunc", "ago", "row",
    "payload"}`. Every activity of the type in the queue has the key
    `schedule:<schedule>:<fire time>`, no fire time twice; there are `count`
    of them, or exactly the fire times `date_trunc(trunc, now) - ago[i]`; each
    has the `runnerq_activities` columns in `row` and the input `payload`.

After the last step the runner also checks every event the scenario wrote
against `schema/postgres/events.schema.json`, and that an event ending an
attempt names its worker.

Expected values are JSON, or matchers: `{"$id": name}`, `{"$token": claim}`,
`{"$null": true}`, `{"$notnull": true}`, `{"$future": true}`,
`{"$past": true}`, `{"$checkpoint": [name, kind, step]}` (a checkpoint id),
`{"$trunc": [unit, interval]}` (the time `date_trunc(unit, now in UTC) -
interval`).
SQL NULL and JSON null are the same.
