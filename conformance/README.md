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
| `submit` | `id`, `type`, `payload`, `priority` (1-4), `max_attempts` (0 unlimited), `timeout_s`, `delay_s`, `metadata`, `parent` (id), `root` (id), `depth`, `key` (`{key, on_duplicate}`: `return_existing`, `no_reuse`, `allow_reuse`, `allow_reuse_on_failure`), `fence` (a claim) | `{}`, or `{"existing": id}` when a returned-existing key matched |
| `claim` | `types`, `limit`, `lease_ms` | `{"claims": [{"id", "token"}]}` in claim order |
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

A claim is `{"id", "token"}` as the driver returned it; drivers keep no state
between requests. Values (`payload`, `value`, `data`) are plain JSON
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
  the id for `submit`, the claims for `claim`. Arguments refer to names:
  activity ids by name, claims by name. `by` picks the driver when there are
  several (`go`, `ts`; default the first). `expect` checks the reply:
  `{"error": kind}`, `{"claims": [names]}` (in order), or any reply field
  (`outcome`, `count`, `renewed`, `existing`, `result`).
- **A fixture** the runner applies in SQL:
  - `{"expire_lease": name}` puts the lease 10 seconds in the past.
  - `{"make_due": name}` sets `scheduled_at` to now.
  - `{"backdate": name, "column": "completed_at", "seconds": n}`.
  - `{"backdate_events": name, "seconds": n}` ages the activity's events.
  - `{"sleep_ms": n}`.
- **A bulk step** the runner performs through drivers:
  - `{"submit_many": {"prefix", "n", "type", "by"}}` submits `n` activities
    named `<prefix>0` to `<prefix>n-1`.
  - `{"drain": {"types", "by": [drivers], "limit", "prefix", "n"}}` claims
    from every listed driver at once until the queue is empty, and checks
    each of the `n` activities was claimed exactly once.
- **An expectation** the runner checks in SQL:
  - `expect_row`: `runnerq_activities` columns of an activity.
  - `expect_events`: the activity's `runnerq_events` types, in insertion order.
  - `expect_result`: a `runnerq_results` row.
  - `expect_absent`: the activity and everything it owns are gone.

After the last step the runner also checks every event the scenario wrote
against `schema/postgres/events.schema.json`, and that an event ending an
attempt names its worker.

Expected values are JSON, or matchers: `{"$id": name}`, `{"$token": claim}`,
`{"$null": true}`, `{"$notnull": true}`, `{"$future": true}`,
`{"$past": true}`, `{"$checkpoint": [name, kind, step]}` (a checkpoint id).
SQL NULL and JSON null are the same.
