# Versions

Version pinning keeps in-flight work on the code that started it. Step keys
are names, so adding or reordering steps is already safe. Pinning covers the
rest: a deploy that changes what happens between steps, where an old workflow
resumed by new code would go wrong.

It is opt-in. A worker with no version stamps nothing and claims as it always
has, so a database, a queue or a fleet that never sets a version behaves
exactly as before.

## The column

`runnerq_activities.code_version` (migration `0004_code_version`) is the
version an activity must run on, or null for unstamped work. A version is any
text of 1 to 256 bytes (a release tag, a commit, a date); versions are compared
for equality only, never ordered.

It is set when the activity is inserted:

| Activity | `code_version` |
| --- | --- |
| A root submitted by a worker engine, or by a client created from one | the engine's version |
| A root submitted by a standalone client | the client's version, if it was given one |
| A child | its root's `code_version` at insert, whatever the spawner passes |
| A schedule's fire or trigger | the schedule's `options.code_version` (below) |
| Anything else | null |

A child takes its root's version, not its spawning worker's: a tree is one
version, so a parent never waits on a child that no worker of its own version
would run. The storage enforces this, reading the root's row when it inserts
the child, so it holds whichever SDK spawned the child.

After insert only a `repin` command changes it. Retries (of either kind),
lease recovery by the reaper, `run_now`, `reschedule`, wakes by a signal, a
child's result or a sleep's end, and an idempotent submission that returns an
existing activity all leave it as it is.

## Workers

A worker has a **version** and **serves** a set of versions:

- `Version` (Go `WorkerConfig.Version`, TypeScript `version`): the version this
  worker stamps on the roots it, and the client it hands out, submit. 1 to 256
  bytes.
- `Serves` (Go `WorkerConfig.Serves`, TypeScript `serves`): further versions it
  claims. It always serves its own version and unstamped work, so the default
  is exactly those. At most 63 entries besides its own.

Serves without a version is a configuration error: an unversioned worker
claims everything anyway.

A worker with a version claims only activities that satisfy

```sql
code_version IS NULL OR code_version = ANY($served)
```

`$served` is its version plus `Serves`. The predicate is one more conjunct of
the claim's eligibility, on single and batch claims alike; nothing else about
the claim changes. A worker without a version adds no predicate.

Parked work (a sleep, a signal wait, a parent waiting on children) resumes by
being claimed again, so it resumes only on a worker that serves its version.
A wake changes the row's status or due time, never its version.

The claim needs no new index: the dequeue indexes are walked in claim order
and the version is checked on each candidate row, as a multi-type claim checks
`activity_type`. `code_version` is in no index, so a repin is a HOT update.

**Unversioned workers claim pinned work.** During a migration from no versions
to versions, a worker without a version still takes everything, stamped or
not. The guarantee holds once every worker of the queue has a version.

## Clients

A client submits roots stamped with its version, or unstamped without one.
A worker engine's client (`engine.Client()`, `worker.client`) has the
engine's version. A standalone client takes one as an option (Go
`runnerq.NewClient(backend, runnerq.ClientVersion("v42"))`, TypeScript
`new RunnerQClient({ ..., version: "v42" })`).

## Schedules

A schedule's fires carry the version stored in the row, as every other fire
option is (schedules.md): `options.code_version`, written at upsert from the
upserting process's version (a worker engine's for schedules it declares, a
client's for its own upserts), and absent when that process has none. Firing
reads only the row, so a fire's version doesn't depend on which worker, in
which language or at which version, fires it.

During a rolling deploy each starting worker upserts its declared schedules,
so the row moves to the new version as the new workers start. A schedule an
old worker re-upserts (a restart mid-rollout) moves back until the next new
worker starts; its fires are always runnable by one side or the other.

## Draining an old version

Two ways to see whether version `A` still has work:

- **The query.** `version` is an activity query field (`eq`, `ne`, `in`, `nin`,
  `exists`; an unstamped activity has none) and an aggregate `group_by` key
  (absent from an unstamped group's key). Unfinished work by version:

  ```json
  {"filter": {"field": "status", "op": "in",
              "value": ["pending", "scheduled", "running", "waiting"]},
   "group_by": ["version"], "metrics": [{"name": "count"}]}
  ```

- **The gauge** `activities_by_version{queue, version}`
  (`observability/metrics.json`): the same counts, sampled by every worker
  that has a version, when gauges are on.

When `A`'s count is 0 and nothing submits as `A` any more, the `A` workers
can stop.

## Stranded work

Work is stranded when its version is pinned and no live worker serves it: it
will never run. No worker can see the whole fleet, so each reports what it
can't serve, and the fleet-wide condition is an aggregate:

- **The gauge** `activities_unserved{queue, version}`: runnable activities (due
  and unclaimed: pending, or scheduled, retrying or waiting with a due time
  that has passed) pinned to a version this worker doesn't serve, per version,
  and 0 for every version it serves.
- **The log line**, at warn, when a worker first sees runnable work for a
  version it doesn't serve (again only after that version's count has gone
  back to 0): `Runnable work is pinned to a version this worker does not
  serve`, with `queue`, `version` and `count`. During a rolling deploy this is
  expected for the old version on new workers and vice versa.

A version is stranded when every worker of the queue reports it unserved: its
minimum across the queue's workers is above 0. A worker that serves it
reports 0 and holds the minimum down. In Prometheus:

```promql
min by (queue, version) (runnerq_activities_unserved) > 0
```

with a `for:` longer than a rollout's overlap (10 minutes, say). Workers
without a version report neither gauge; while some run, they take pinned work
too (above), so the rule may fire for work they will in fact claim.

## Moving work on purpose: repin

When a team decides old work is safe on new code, the `repin` command moves it
(the command API: Go `Client.Repin`, TypeScript `client.repin`). It goes
through the same audited path as cancel and retry: the `runnerq_commands`
ledger, a `command_id` replaying its result, `dry_run`, `reason`.

- **Input:** a target (ids, or a filter with `max`) and `version`, the version
  to pin to (1 to 256 bytes, required). Fingerprint member `version`
  (`vectors/command_fingerprint.json`).
- **Acts on** unfinished activities (pending, scheduled, retrying, waiting,
  running) whose version differs from the target, unstamped ones included. A
  finished activity, or one already on the target version, is skipped with
  `conflict`; a filter target selects only activities it can act on, so
  repeating a bounded repin works through the rest.
- **Effect:** sets `code_version`, records a `Repinned` event (`from`, `to`),
  and changes nothing else. A running attempt finishes on the worker running
  it; whatever that activity does next (a retry, a wake) is claimed under the
  new version.
- **Per activity**, not per tree. A child spawned after its root is repinned
  takes the root's new version. To move a workflow, target its tree
  (`root_id`); to move a version, filter on it (`version eq A`).

There is no repin to unstamped: to let every worker take some work, pin it to
a version they all serve.

## Executors

A worker with a version reports it as its executor label `version`
(overriding an application label of that name), on every path that reports
executors (the conductor's `hello`, hosted workers' executor reports). Without
one, labels are as the application set them.

## A rolling deploy from A to B

1. Every worker of the queue runs with a version; today's run `A`.
2. Start workers at `B` (serving `B` and unstamped work). New roots they, and
   producers built at `B`, submit are stamped `B`; producers still at `A`
   stamp `A`, which the `A` workers keep running.
3. Move the producers to `B`.
4. Watch `activities_by_version{version="A"}` (or the query). At 0, stop the
   `A` workers.
5. Alert on stranded work throughout (above).

If `A`'s work turns out to be safe on `B`, either repin it
(`version eq A` → `B`), or run some `B` workers that also serve `A`
(`Serves: ["A"]`) and stop the `A` workers.

Rolling back from `B` to `A` is the same with the roles swapped.
