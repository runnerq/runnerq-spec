# Chaos harness

`tools/chaos` runs a seeded workload on a fleet of SDK worker processes. While
the workload runs, it kills workers, pauses them for longer than their lease and
restarts Postgres. When the workload has finished, it checks in SQL that nothing
was lost and that no recorded step ran twice.

```sh
cd tools/chaos
go run . -dsn "$RUNNERQ_TEST_DSN" -worker go=/path/to/chaosworker
go run . -dsn "$RUNNERQ_TEST_DSN" -worker ts="node …/runnerq-ts/test/chaos-worker.mjs"
# a mixed fleet, every fault
go run . -dsn "$RUNNERQ_TEST_DSN" -n 4 \
  -worker go=/path/to/chaosworker -worker ts="node …/chaos-worker.mjs" \
  -faults kill,pause,pg -pg-restart 'docker restart runnerq-chaos-pg'
```

Each run uses a new queue, so one database can hold many runs. The ledger table
`runnerq_chaos_ledger` is created if it is missing. Do not point `-pg-restart`
at a database that other people or test suites use.

| Flag | Default | |
| --- | --- | --- |
| `-seed` | random | Decides the workload, which SDK enqueues and signals each root, and the fault schedule. |
| `-worker sdk=command` | | Repeatable, once per SDK. `sdk` is also the prefix of the activity types the SDK's workers serve. |
| `-n` | 3 | Worker processes in all, spread round-robin over the SDKs. |
| `-roots` | 40 | Root activities. They are enqueued during the first half of `-duration`. |
| `-duration` | 60s | How long faults are injected. |
| `-faults` | `kill` | `kill`, `pause` and `pg`, comma-separated. Repeating a fault weights it. |
| `-fault-every` | 4s | Mean time between faults (exponential). |
| `-pause` | 75s | How long a paused worker stays stopped. It must be longer than every SDK's lease. The Go SDK extends a lease 60s at a time. |
| `-pg-restart` | | Shell command that restarts Postgres, for `pg`. The harness waits until Postgres accepts queries again. |
| `-lease` | 15s | Passed to workers as `RUNNERQ_CHAOS_LEASE_MS`. |
| `-settle` | 5m | How long the workload may take to finish after the faults. |
| `-logs` | temp dir | Worker and client stderr, one file per worker slot. |
| `-rolling` | off | A rolling deploy (below). |

A failing run prints its seed and the command that replays it. Process timing is
not seeded, so a replay injects the same faults at the same times, but they may
land in different instructions.

## Faults

- **kill**: SIGKILL a running worker, then start it again after up to 1.5s.
- **pause**: SIGSTOP a worker for `-pause`, then SIGCONT it. The harness always
  leaves at least one worker running. A paused worker's claims expire, other
  workers take them over, and the stale worker then wakes up in the middle of
  its work.
- **pg**: run `-pg-restart`. There are at least 15s between restarts.

Signals go to the worker's process group, so a command that starts the SDK
process through a shell, `go run` or `npx` still works.

## Invariants

After the faults stop, every worker is brought back up. The harness waits until
every planned root has been enqueued and every activity in the queue is
terminal. Then it stops the fleet and checks:

1. **Nothing is lost.** Every planned root exists exactly once, every activity
   completed with an `Ok` result, and every parent has exactly the children it
   spawned (one per step name, none twice).
2. **No recorded step's effect happens twice.** Every step has a checkpoint.
   The value it recorded is the nonce of one of that step's ledger effects. No
   effect of the step began after the checkpoint was recorded. An effect that
   landed before the checkpoint is an allowed re-execution, because steps run
   at least once. An effect that landed after it, from a worker that was paused
   at the time, is counted as a stale execution. Fencing that is a later goal;
   from any other worker it is a failure.
3. **Results are consistent.** The activity's result carries the recorded step
   values (replays returned the stored values), the signal payload that was
   sent, and each child's own result.
4. **Parked work woke exactly once.** The number of handler invocations is at
   most 1 + the activity's `Yielded`, `Requeued` and `Retrying` events. Every
   extra attempt needs a park, a lease expiry or a retry to explain it. Nothing
   invoked the handler after the activity completed. The steps after a wait
   began only after it ended: after the sleep's stored `wake_at` (allowing 1s
   of clock skew between worker and database), after the `Signaled` event, or
   after the last child completed. A waiter has between 1 and (sends
   attempted) `Signaled` events.

## Rolling deploy

With `-rolling`, the run is a version change from `A` to `B`
([versions.md](../../versions.md)). The `-n` workers and the clients start at
version `A`. A quarter into the fault window, `-n` more workers start at `B`
and the clients restart at `B`, so later roots are pinned to `B`. Once no
unfinished activity is pinned to `A`, the `A` workers stop for good. Faults
hit both generations throughout.

On top of the invariants above, every activity must be pinned to `A` or `B`,
and every ledger row of an activity (each invocation and step effect) must come
from a worker of its version: a worker's label carries its version
(`go@A-0.3`). A run with no activity at either version fails, as it tested
nothing.

## Worker contract

An SDK provides one program. The harness runs it as `sh -c "exec <command> <mode>"`
in its own process group, with `<mode>` `work` or `client`, and with this
environment:

| Variable | |
| --- | --- |
| `RUNNERQ_CHAOS_DSN` | Postgres connection string. |
| `RUNNERQ_CHAOS_QUEUE` | The queue to use. |
| `RUNNERQ_CHAOS_PREFIX` | The SDK's name from `-worker`. It serves, enqueues and spawns activity types `<prefix>.flow` (and any `<other>.flow` named in a payload). |
| `RUNNERQ_CHAOS_WORKER` | This incarnation's label, such as `go-1.3`. It is written to every ledger row. |
| `RUNNERQ_CHAOS_CONCURRENCY` | `work`: activities run at once. |
| `RUNNERQ_CHAOS_LEASE_MS` | `work`: the claim lease, if the SDK can set it. |
| `RUNNERQ_CHAOS_VERSION` | Set only by `-rolling`. `work`: the worker's code version (it serves that version and unstamped work). `client`: the version roots are stamped with. |

### `work`

Initialize or check the schema, start a worker serving `<prefix>.flow`, and
print `{"ready":true}` on stdout once it is claiming. On SIGTERM, shut down
gracefully and exit 0. Exiting at any other time is a failure. Log to stderr.

The `<prefix>.flow` handler takes this payload (plain JSON):

```json
{
  "label": "root-7",
  "steps": ["s1", "s2"],
  "step_ms": 120,
  "sleep_ms": 14000,
  "signal": "go-ahead",
  "children": [{"type": "ts.flow", "payload": {…}, "timeout_s": 10, "max_retry_delay_s": 2}],
  "after": ["after"]
}
```

At most one of `sleep_ms`, `signal` and `children` is set. The handler:

1. Inserts an `invoke` row: `INSERT INTO runnerq_chaos_ledger (queue_name, activity_id, kind, worker) VALUES ($queue, $id, 'invoke', $worker)`.
   This is not a step, so it runs on every invocation.
2. Runs each of `steps` as a checkpointed step with that name (`run:<name>`).
   The step body is the **effect**:
   - `SELECT clock_timestamp()` → `begun`;
   - wait `step_ms / 2`;
   - `INSERT INTO runnerq_chaos_ledger (queue_name, activity_id, kind, step, nonce, worker, begun_at) VALUES ($queue, $id, 'effect', $step, $nonce, $worker, $begun)` with a fresh random UUID `nonce`;
   - wait `step_ms / 2`;
   - return `nonce` (a JSON string).

   Database errors are retryable failures.
3. Waits once:
   - `sleep_ms`: a durable sleep named `nap`;
   - `signal`: waits, with no timeout, for the signal with that name;
   - `children`: spawns each child as a step-named child `child-<i>` of the
     given type and payload, with unlimited attempts and the given timeout and
     maximum retry delay. It then awaits them all.
4. Runs each of `after` as in step 2.
5. Returns `{"label", "steps": {name: nonce}, "signal": <payload>, "children": [<child results>]}`.
   It omits `signal` and `children` when they are unused.

Payloads, results and signals cross SDKs, so they must be stored as plain JSON
(`json-v1`). Step checkpoints may use the SDK's own serialization, because the
harness decodes both. Only the SDK that owns a `<prefix>.flow` type runs it. The
TypeScript SDK documents that Go must not resume its native checkpoints, so a
mixed fleet shares queues, children and signals, but not handlers.

### `client`

Initialize the schema, print `{"ready":true}`, then answer one JSON request per
stdin line with one line on stdout: `{"ok": …}` or `{"error": "message"}`. Exit
when stdin closes.

| Request | Reply |
| --- | --- |
| `{"op": "enqueue", "type", "key", "payload", "timeout_s", "max_retry_delay_s"}` | `{"ok": {"id": "<uuid>"}}`. Enqueue a root with idempotency key `key` (return existing), unlimited attempts and the given timeout and maximum retry delay. The harness retries failed calls, so a retry must return the same activity. |
| `{"op": "signal", "id", "name", "payload"}` | `{"ok": {}}`. Deliver the signal, as plain JSON. |

The harness restarts a client that dies, for example when Postgres restarts
under it.

## Implementations

- Go: `runnerq-go/internal/chaosworker` (`go build -o chaosworker ./internal/chaosworker`).
- TypeScript: `runnerq-ts/test/chaos-worker.mjs` (after `npm run build`).
