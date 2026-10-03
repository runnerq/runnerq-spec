# RunnerQ Conductor protocol (v1)

The contract between a RunnerQ SDK **agent** (running inside a worker process)
and the RunnerQ Cloud **gateway**.

This spec is designed from what the Cloud needs, now and as it grows. It does
not mirror any SDK's internals: every SDK maps its own storage onto the
resources, queries and commands defined here.

| File | What it is |
| --- | --- |
| `conductor.schema.json` | Every message (`x-messages`: its kind, sender, data and response) and every type, as JSON Schema. |
| `examples/<type>.json` | One example of each message, with its response. |
| `executor_report.example.json` | A hosted worker's report (§8). |

The schema is the contract; this document explains it. `verify/` checks every
example against the schema. The Cloud, both SDKs' agents and the storage
adapters use types generated from it (`tools/gen -part conductor`, and
`-part executor-report` for the adapters), and implementations can check what
they send with the `schemacheck` package.

## 1. Principles

1. **Agents dial out.** The Cloud never connects into customer networks and
   never holds database credentials.
2. **Off the execution path.** Nothing in this protocol can slow or stop
   activity execution. Agents run in worker processes only.
3. **Closed command set.** The Cloud can only ask for the operations defined
   here, and agents execute them against their own storage.
4. **The spec owns the shapes.** Resources have canonical, language-neutral
   schemas. SDK-specific data goes in `extensions`, never in the core fields.
5. **Room to grow.** New operations, filters, fields and aggregates are added
   without a protocol version bump, negotiated per message through
   capabilities (§4). The Cloud must never assume a feature it was not
   offered.
6. **Queries, not views.** Reads are general queries (filter, sort, project,
   paginate, aggregate) rather than one message per screen. New console
   screens should not need new messages.
7. **Data stays home in metadata-only mode.** The agent enforces redaction
   before anything leaves the process.

## 2. Transport

- WebSocket over TLS: `GET wss://<host>/v1/agent`.
- `Authorization: Bearer <api-key>` on the upgrade request. The key identifies
  the app. Keys never appear in URLs.
- One text frame carries one JSON envelope. Frames are UTF-8 JSON; binary
  frames are reserved.
- Both sides ping every 20s and treat 30s without any inbound frame as a dead
  connection.
- Agents reconnect with exponential backoff (1s → 30s cap, ±50% jitter). A
  close with status 1001 (going away) means the gateway node is draining:
  reconnect promptly. Revoking the agent key closes its sessions within
  seconds, with status 1008 (policy violation) and reason `key_revoked`;
  reconnecting then fails with 401 until the worker has a valid key.
- Maximum frame size is negotiated in the handshake (`limits`); the default is
  4 MiB.

## 3. Envelope

```json
{"v":1, "kind":"req", "id":"42", "type":"activities.list", "data":{...}, "meta":{"traceparent":"00-…"}}
{"v":1, "kind":"res", "id":"42", "type":"activities.list", "data":{...}}
{"v":1, "kind":"res", "id":"42", "type":"activities.list", "error":{"code":"invalid_argument","message":"…","details":{…}}}
{"v":1, "kind":"evt", "type":"executor.report", "data":{...}}
```

| Field | Meaning |
|---|---|
| `v` | Protocol version from the handshake. |
| `kind` | `req` expects a `res`; `evt` is one-way. |
| `id` | Set on `req`, echoed on its `res`. Unique per sender per connection. |
| `type` | Message type, namespaced `resource.verb`. A `res` echoes its request's type. |
| `data` | Payload for the type. |
| `error` | Set instead of `data` on a failed `res`. |
| `meta` | Optional, open map for cross-cutting concerns (trace context, deadlines). Unknown keys are ignored. |

Requests may go either way. Today the gateway sends queries and commands and
the agent sends the handshake; the envelope does not assume a direction.

### Deadlines

`meta.deadline` (RFC 3339) is the time after which the caller will not use
the reply. Agents should stop work past it and must not start work that
already expired.

### Errors

| Code | Meaning |
|---|---|
| `invalid_argument` | Malformed or out-of-range input. `details.field` names it. |
| `not_found` | The target does not exist. |
| `failed_precondition` | The target is in the wrong state (e.g. cancelling a completed activity). `details.status` gives its state. |
| `conflict` | A concurrent change won (e.g. the command's `command_id` was already applied with different input). |
| `forbidden` | Disallowed by agent policy (control disabled, metadata-only). |
| `unsupported` | Not implemented by this agent: an operation, filter, sort, field, group-by or metric it did not advertise. |
| `resource_exhausted` | Agent is at its concurrency limit, or the reply would exceed the frame limit. |
| `deadline_exceeded` | The request ran past its deadline. |
| `unavailable` | Storage is temporarily unreachable; safe to retry. |
| `internal` | Anything else. |

Agents must reject rather than ignore anything they do not understand in a
request. Silently dropping an unknown filter would return wrong data.

## 4. Handshake and capabilities

The agent's first frame must be `hello` (a `req`) within 10s. The gateway
answers `hello` with a welcome, or with an error and a 1008 close.

```json
{"v":1,"kind":"req","id":"1","type":"hello","data":{
  "protocol_versions":[1],
  "sdk":{"name":"runnerq-go","version":"0.8.0","language":"go"},
  "executor":{
    "id":"6f1c…",
    "hostname":"billing-7d9f-x2",
    "queues":["default"],
    "activity_types":["charge_card","send_invoice"],
    "max_concurrency":32,
    "started_at":"2026-09-27T10:00:00Z",
    "labels":{"region":"eu-west-1","version":"2026.09.27-3"}
  },
  "capabilities":{
    "activities.list":{"v":1,"filters":["status","type","queue","root","parent_id","idempotency_key","metadata","created_at","updated_at","completed_at","text"],"sorts":["created_at","updated_at","completed_at"],"include":["payload","last_error","result"]},
    "activities.get":{"v":1,"include":["payload","last_error","result","steps","events"]},
    "activities.aggregate":{"v":1,"group_by":["status","type","queue","root"],"buckets":["created_at","completed_at"],"metrics":["count","duration.queue","duration.run","duration.total"]},
    "activities.cancel":{"v":1,"targets":["ids","filter"]},
    "events.subscribe":{"v":1}
  },
  "limits":{"max_frame_bytes":4194304,"max_concurrent_requests":16}
}}
```

```json
{"v":1,"kind":"res","id":"1","type":"hello","data":{
  "version":1,
  "session_id":"b0e2…",
  "app":{"id":"…","name":"billing"},
  "config":{"data_mode":"full","report_interval_ms":15000},
  "limits":{"max_frame_bytes":4194304}
}}
```

- **`capabilities`** is a map keyed by message type. Its value lists the
  message's schema version (`v`) and any sub-features the agent supports. The
  gateway only sends what is advertised, and only with advertised
  sub-features. The Cloud UI greys out anything no connected executor
  supports. An agent also advertises the events it can send when the gateway
  asks for them (`activity.notices`).
- **Message versions** (`v` inside a capability) let one message evolve
  incompatibly (`activities.list` v2) while the rest of the protocol stays at
  protocol `v1`. Requests carry the chosen message version in `meta.mv` when
  it is above 1.
- **`executor.id`** is stable for the life of the worker process. If an
  executor reconnects while its previous session is open, the newest session
  wins and the old one closes with reason `superseded`.
- **`labels`** are free-form executor tags (region, deploy version). The Cloud
  can filter and group executors by them.
- **`config`** is the gateway's settings for this session. The gateway may
  change it at any time with a `config.update` event (§9), so settings never
  require a reconnect.
- The effective frame limit is the smaller of the two sides' `limits`.

## 5. Resources

Timestamps are RFC 3339 in UTC with millisecond precision. Durations are
integer milliseconds and named `*_ms`. IDs are opaque strings: the Cloud must
not assume UUIDs. Fields that do not apply are omitted, not null.

### Activity

```json
{
  "id": "…",
  "type": "charge_card",
  "queue": "default",
  "status": "running",
  "priority": 2,
  "root_id": "…",
  "parent_id": "…",
  "depth": 1,
  "idempotency_key": "order-42",
  "attempt": 2,
  "max_attempts": 4,
  "created_at": "…",
  "scheduled_for": "…",
  "started_at": "…",
  "completed_at": "…",
  "updated_at": "…",
  "timeout_ms": 300000,
  "lease_expires_at": "…",
  "executor_id": "…",
  "wait": {"kind": "signal", "name": "approve", "until": "…"},
  "metadata": {"tenant": "acme"},
  "last_error": {"message": "…", "kind": "retryable", "at": "…"},
  "payload": {…},
  "result": {"state": "ok", "data": {…}},
  "extensions": {"go": {…}}
}
```

| Field | Notes |
|---|---|
| `status` | One of the canonical statuses below. |
| `attempt` | The current or last attempt, starting at 1. |
| `max_attempts` | Total attempts allowed. Absent when unlimited. |
| `executor_id` | The executor currently running it, if running. |
| `wait` | Present while `status` is `waiting`. `kind` ∈ `sleep`, `signal`, `children`, `other`. |
| `payload`, `result`, `last_error` | Returned only when requested with `include` (§6.1), and never in metadata-only mode. |
| `extensions` | SDK-specific data, keyed by SDK language. The Cloud may display it but must never depend on it. |

**Canonical statuses:**

| Status | Meaning | Terminal |
|---|---|---|
| `pending` | Ready to run, waiting for a worker. | |
| `scheduled` | Will become `pending` at `scheduled_for` (delayed or awaiting a retry backoff). | |
| `running` | Claimed by an executor. | |
| `waiting` | Parked on a sleep, signal or children without holding a worker. | |
| `completed` | Succeeded. | ✓ |
| `failed` | Failed permanently without dead-lettering. | ✓ |
| `dead_letter` | Exhausted retries or failed non-retryably, and was dead-lettered. | ✓ |
| `cancelled` | Cancelled by a command. | ✓ |

A separate `retrying` status is deliberately not part of the model. A retry
is `scheduled` with `attempt > 1`, which the query language can express
(`status: scheduled`, `attempt: {gt: 1}`).

### Step

A durable checkpoint inside an activity.

```json
{"id":"…","activity_id":"…","name":"charge","kind":"run","status":"completed",
 "created_at":"…","completed_at":"…","child_id":"…","result":{"state":"ok","data":{…}}}
```

- `kind` ∈ `run`, `sleep`, `signal`, `child`, `other`.
- `child_id` is present when the step spawned a child activity.
- `result` is included on request only.

### Event

An entry in an activity's lifecycle history.

```json
{"id":"1203","cursor":"1203","activity_id":"…","type":"attempt.failed","at":"…",
 "executor_id":"…","attempt":2,"detail":{…}}
```

Events are stored only for what an activity can't show itself
(`schema/postgres/events.schema.json`): an attempt ending other than in
success, waits, signals, links and commands. `activity.created`,
`activity.scheduled`, `attempt.started` and `attempt.succeeded` are an
activity's `created_at`, `scheduled_for`, `started_at` and `completed_at`, and
are never in `events.list` or a stream: they are announced as notices
(section 9).

Event types:

| Group | Types |
|---|---|
| `activity` | `created`, `scheduled`, `cancelled`, `retried`, `run_now`, `rescheduled`, `priority_changed` |
| `attempt` | `started`, `succeeded`, `failed`, `timed_out`, `lease_expired`, `lease_extended` |
| `wait` | `parked`, `woken` |
| `signal` | `received` |
| `step` | `completed` |
| `child` | `spawned`, `linked` |
| `result` | `stored` |
| `dead_letter` | `entered`, `redriven` |

Unknown types must be displayed generically, not rejected. `detail` is open
and is omitted in metadata-only mode.

### Status mapping notes

SDKs map their internal states onto the canonical set. For example, the Go
SDK's `processing` is `running`, and its `retrying` is `scheduled` with
`attempt > 1`.

### Result

```json
{"state":"ok","data":{…}}
{"state":"error","error":{"message":"…","kind":"non_retryable"}}
```

## 6. Queries

### 6.1 Common query shape

Every list-style request uses the same building blocks.

```json
{
  "filter": { … },
  "sort": [{"field":"created_at","order":"desc"}],
  "include": ["payload"],
  "limit": 50,
  "cursor": "…"
}
```

**`filter`** is a boolean expression over a resource's fields:

```json
{"and":[
  {"field":"status","op":"in","value":["scheduled","running"]},
  {"field":"type","op":"eq","value":"charge_card"},
  {"field":"created_at","op":"gte","value":"2026-09-01T00:00:00Z"},
  {"field":"metadata.tenant","op":"eq","value":"acme"},
  {"not":{"field":"parent_id","op":"exists"}}
]}
```

- Combinators: `and`, `or`, `not`.
- Operators: `eq`, `ne`, `in`, `nin`, `lt`, `lte`, `gt`, `gte`, `exists`, and
  `prefix` / `contains` for strings (`contains` is gated by the `text`
  capability).
- Fields are dotted paths into the resource schema. `metadata.<key>` addresses
  metadata tags.
- An agent answers `unsupported` with `details.field` for any field, operator
  or combination it cannot evaluate efficiently. The advertised `filters`
  list says which fields are queryable at all.

**`sort`** fields must be advertised. The agent always adds a unique
tiebreaker (the ID) so paging is stable.

**`include`** asks for heavy fields (`payload`, `result`, `last_error`, and on
`activities.get` also `steps` and `events`). They are never returned unless
requested, which keeps lists cheap.

**Pagination** is cursor-based. The reply carries `next_cursor` when more rows
exist. Cursors are opaque to the Cloud, stable across concurrent inserts, and
valid for at least one hour. `limit` defaults to 50 and is capped at 1000.

**Consistency:** each page reflects committed state when it was read. There is
no snapshot isolation across pages.

### 6.2 Messages

| Type | Request | Response |
|---|---|---|
| `activities.list` | common query | `{"items":[Activity], "next_cursor"?}` (`ActivityPage`) |
| `activities.get` | `{"id", "include"?}` | `Activity`, with `steps` and `events` when included (an included empty list is `[]`) |
| `activities.count` | `{"filter"}` | `{"count", "exact": bool}`; agents may return an estimate over large sets and say so |
| `activities.aggregate` | see §6.3 | see §6.3 |
| `steps.list` | `{"activity_id", "include"?, "limit"?, "cursor"?}` | `{"items":[Step], "next_cursor"?}` (`StepPage`) |
| `events.list` | common query over events (filters include `activity_id`, `root_id`, `type`, `at`), sorted by `at` | `{"items":[Event], "next_cursor"?}` (`EventPage`) |
| `results.get` | `{"activity_id"}` | `Result` |
| `trees.get` | `{"id", "include"?, "max_nodes"?}`: `id` may be any activity in the tree | `{"root_id", "items":[Activity], "truncated": bool}` |
| `executor.describe` | `{}` | Live state of this executor (§8) |

Children of an activity are an `activities.list` filtered by `parent_id`; the
dead-letter queue is `status: dead_letter`; recent workflows are
`not exists parent_id` sorted by `created_at`. No message exists for any
particular screen.

### 6.3 Aggregates

One general message serves dashboards, charts and alert evaluation.

```json
{"type":"activities.aggregate","data":{
  "filter":{…},
  "group_by":["type","status"],
  "bucket":{"field":"completed_at","interval_ms":60000,"from":"…","to":"…"},
  "metrics":[{"name":"count"},{"name":"duration","field":"run","percentiles":[50,95,99]}],
  "limit":200
}}
```

```json
{"groups":[
  {"key":{"type":"charge_card","status":"completed"},"bucket":"2026-09-27T10:01:00Z",
   "count":1204,"durations":{"run":{"p50":180,"p95":900,"p99":2400}}}
],"truncated":false}
```

- `group_by`, `bucket.field` and each metric must be advertised. Duration
  metrics are advertised per field (`duration.run`).
- Duration fields:
  - `queue`: from `created_at` or `scheduled_for` to the first start.
  - `run`: from the start of the last attempt to completion.
  - `total`: from `created_at` to completion.
- Aggregates are computed from the database for the requested window, so any
  executor gives the same answer. The Cloud uses them for usage metering too:
  it asks for completed-activity and step counts per window.

## 7. Commands

Commands change state. The Cloud audits every command, whatever the result.

### 7.1 Common command shape

```json
{
  "command_id": "cmd_01J…",
  "target": {"ids":["…","…"]},
  "dry_run": false,
  "reason": "customer asked to stop the rollout"
}
```

- **`command_id`** makes commands idempotent. The agent records applied
  command IDs (with their outcome) for at least 24 hours, and replays the
  stored outcome if the same ID arrives again. Same ID with different input
  is a `conflict`.
- **`target`** is `{"ids":[…]}` (up to 1000), `{"filter":{…}, "max": N}` for
  bulk actions such as "retry every dead-lettered `charge_card` from today",
  or `{"idempotency_key", "type"}` for signals. A filter target must set
  `max`. It selects only activities the command can act on (non-terminal
  ones for cancel, failed ones for retry, …), so repeating a bounded command
  works through the rest; `more` says whether more remain.
- **`target.queue`**: agents apply commands to activities of the queue they
  serve, because acknowledgements, wake-ups and results are queue-scoped.
  The Cloud routes each command to an executor serving the queue and names
  it here; a mismatch is `failed_precondition`. (The Cloud's HTTP API takes
  commands without a queue and fans them out per queue: ids by looking up
  their queues, filters across the live queues sharing `max`, keys to every
  queue.)
- **`dry_run`** returns what would happen without changing anything.
- **`reason`** is free text, recorded in the activity's event history.

**Response:**

```json
{"matched": 3, "applied": 2, "cascaded": 4, "more": false, "replayed": false,
 "results":[
   {"id":"…","outcome":"applied","status":"cancelled"},
   {"id":"…","outcome":"skipped","error":{"code":"failed_precondition","message":"already completed","details":{"status":"completed"}}}
 ]}
```

Per-item failures do not fail the request. A request-level `error` means
nothing was applied. `cascaded` counts descendants a cascading cancel also
cancelled; `replayed` marks a result served from the command ledger.

### 7.2 Commands

Each command has its own request type (`CancelRequest`, `RetryRequest`, …):
the common fields above plus its own.

| Type | Extra fields | Effect |
|---|---|---|
| `activities.cancel` | `{"cascade":"children"\|"none"}` (default `children`) | Cancel non-terminal activities, and by default their non-terminal descendants. A running one is stopped through its claim (at once on the executor that received the command, otherwise at its next heartbeat). A cancellation error result wakes anything awaiting it, so a waiting parent sees a cancellation error. |
| `activities.retry` | `{"reset_attempts": bool}` | Run failed, dead-lettered or cancelled activities again. Their checkpoints are kept, so completed steps are replayed rather than repeated. |
| `activities.run_now` | — | Make `scheduled` (including retry backoff) or `waiting` activities runnable immediately. |
| `activities.reschedule` | `{"at"}` | Move a `scheduled` activity's time. |
| `activities.set_priority` | `{"priority"}` | Change priority of non-terminal activities. |
| `activities.delete` | `{"cascade":"tree"}` | Delete a finished root together with its whole tree and history. Non-roots, trees with running work, and trees another live workflow depends on are skipped. |
| `activities.signal` | `{"name","payload"}` | Deliver a signal and wake the activity if it is waiting. Finished activities are skipped. |

Each command accepts only its own fields; anything else is
`invalid_argument`. Agents advertise commands only when they are allowed to
run them (a read-only agent advertises none).

## 8. Executors

`executor.describe` returns the live, in-memory state that only a worker
process has, and which a database cannot provide:

```json
{"id":"…","uptime_ms":…,"max_concurrency":32,"in_flight":7,
 "running":[{"activity_id":"…","type":"charge_card","attempt":1,"started_at":"…"}],
 "claim_lag_ms":12,"heartbeat_failures":0,"draining":false,
 "counters":{"claimed":1204,"succeeded":1180,"retried":17,"failed":3,
             "timed_out":1,"dead_lettered":2,"claims_lost":0}}
```

`claim_lag_ms` is how long the latest activity waited, from when it was due,
to start; `counters` count outcomes since the executor started. All of them
are always present. The agent also pushes the headline numbers (everything but
`running`) as an `executor.report` event every `config.report_interval_ms`,
so dashboards do not poll.

A hosted app's workers have no agent: their storage adapter reports the same
shapes to the data plane instead, `{"sdk":…,"executor":…,"state":…}` (the
hello's `sdk` and `executor`, and the state above with `running`), with the
store key to `PUT /v1/executors/{id}` every 10 seconds, and `DELETE` on a
clean stop (`ExecutorReport`). The control plane shows both kinds of executor
the same way.

## 9. Events and streams

**Agent → gateway:**

| Type | Data | Meaning |
|---|---|---|
| `goodbye` | `{"reason"}` | Graceful shutdown, so a deploy is not recorded as a crash. |
| `executor.report` | subset of `executor.describe` | Periodic live metrics. |
| `stream.events` | `{"subscription_id","items":[Event],"cursor"}` | Batched events for a subscription. |
| `stream.gap` | `{"subscription_id","since_cursor"}` | Events may have been lost; the Cloud re-queries from `since_cursor` with `events.list`. |
| `activity.notices` | `{"items":[Notice],"dropped"?}` | Lifecycle changes this executor made, while `config.notices` is on. |

**Gateway → agent:**

| Type | Kind | Data |
|---|---|---|
| `config.update` | evt | A partial `config`; the agent applies it immediately. |
| `events.subscribe` | req | `{"filter","after_cursor"?,"max_batch"?,"max_delay_ms"?}` → `{"subscription_id","cursor"}` (where the stream starts) |
| `events.unsubscribe` | req | `{"subscription_id"}` → `{}` |

- Subscriptions resume from `after_cursor`, so a reconnect or failover to
  another executor loses nothing: the new subscriber starts from the last
  delivered cursor. Without `after_cursor` a stream starts at the end of the
  log. An `after_cursor` the agent cannot interpret starts at the end and is
  answered with `stream.gap`.
- Event cursors are ordered and shared across the app's executors, so any
  executor can resume any subscription. Events are filterable by `seq` (the
  log position), `queue`, `activity_id`, `root_id`, `type`, `at` and
  `executor_id`.
- Delivery is at least once: agents rescan a window below the cursor for
  events whose transaction committed late, and consumers drop repeats by
  event `id`.
- Subscriptions belong to the session and end with it. Agents allow a few
  per session (`resource_exhausted` beyond).
- The gateway keeps one subscription per app, whatever the number of
  viewers, and fans out to browsers itself; it re-subscribes elsewhere from
  the last cursor when the serving executor goes away.

### Notices

Stored events cover only what an activity can't show itself, so the four
changes every activity goes through (`activity.created`, `activity.scheduled`,
`attempt.started`, `attempt.succeeded`) are announced live instead, by the
executor that made them:

```json
{"activity_id":"…","type":"attempt.started","at":"…","queue":"payments",
 "activity_type":"charge_card","root_id":"…","attempt":1,"executor_id":"exec-7f3a"}
```

- An agent that can send them advertises the capability `activity.notices`
  in `hello`. It sends them only while `config.notices` is on: the gateway
  turns it on while someone is watching the app, and off again.
- Every executor sends its own; the gateway gathers them from all of the
  app's sessions. A submission is announced when an executor's process makes
  it (a handler spawning a child, or code using the engine); a process with no
  agent announces nothing.
- Best effort, never stored and never resent: notices have no cursor, a gap
  or reconnect loses the ones in between, and a consumer that needs the truth
  reads the activity. Agents send a batch at most every 250 ms, keep at most a
  few thousand waiting, and drop the oldest beyond that, reporting how many in
  the next batch's `dropped`.
- Metadata-only mode doesn't change them: they carry no payload or detail.

## 10. Metadata-only mode

Active when the welcome or a `config.update` sets `data_mode:
"metadata_only"`, or when the agent is configured locally to force it. The
local setting always wins.

- Omitted:
  - `payload`, `result` and `last_error.message` on activities;
  - `result` on steps;
  - `detail` on events;
  - signal payloads in history.
- `results.get` and any `include` of a redacted field answer `forbidden`.
- Commands still work, and `activities.signal` may carry a payload inbound.

## 11. Versioning rules

- New message types, capabilities, sub-features, optional request fields and
  response fields are additive and need no version change.
- Receivers ignore unknown response fields and unknown `meta` keys.
- Agents reject (`unsupported`) unknown request fields that change meaning,
  such as filters, sorts, includes and targets.
- An incompatible change to one message bumps that message's `v` in its
  capability. Both versions can be served side by side.
- A change to the envelope or handshake bumps the protocol version. The
  gateway picks the highest version both sides list.

Starting new work from the Cloud (an `activities.enqueue` command) is
deliberately out of scope: the Cloud may inspect and steer existing work, not
create it.

## 12. What agents implement first

The spec describes the full surface so later work does not force breaking
changes. A first agent release needs only:

- `hello` and `goodbye`
- `activities.list`, `activities.get`, `activities.count`
- `steps.list`, `events.list`, `results.get`, `trees.get`
- `activities.aggregate` (count by status and type, with time buckets)
- `executor.describe`
- the `activities.*` commands, when the agent allows control

Everything else ships behind capabilities as the Cloud grows.
