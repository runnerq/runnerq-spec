# Storage protocol v1

How a worker's storage adapter (cloud-storage-go, cloud-storage-ts) talks to
the RunnerQ data plane (`storaged`). This is separate from the conductor
protocol between a worker's agent and the control plane.

| File | What it is |
| --- | --- |
| `storage.schema.json` | Every operation's arguments and result (`x-operations`), and the types they carry. |
| `examples/<Operation>.json` | One request and response per operation, as the Go adapter encodes them. |

A worker's heartbeat (below) is the conductor protocol's `ExecutorReport`.

`verify/` checks every example against its schema. The adapters and storaged
use code generated from the schemas (`tools/gen -part storage` and
`-part executor-report`).

## Requests

```text
POST /v1/queues/{queue}/{operation}
Authorization: Bearer <store key>
RunnerQ-Storage-Version: 1
Content-Type: application/json
```

The body is the operation's arguments (`<Operation>Args`). The key resolves
its store; the URL names the queue in it. A queue is recorded the first time a
key uses it. Queue names follow the SDKs' rule: 1 to 48 letters, digits and
underscores, starting with a letter or underscore. A body cannot choose a
store, schema or other queue.

A store key (`rqh_…`) grants full storage access to every queue in its store.

## Encoding

The wire is the Go SDK's JSON encoding of its storage types, so:

- Argument fields are the camelCase names in each `<Operation>Args`.
- The records inside (`QueuedActivity`, `ActivityResult`, `FailureKind`, …)
  use capitalized Go field names; read models (`ActivitySnapshot`,
  `ActivityEvent`, `DeadLetterRecord`) use snake_case.
- UUIDs are lowercase strings; times are RFC 3339 with up to nanosecond
  precision; durations are integer nanoseconds; payloads, results and details
  are JSON as is.
- A nullable field may be absent: absent and null mean the same.
- String lengths in the schema are UTF-8 bytes.

64-bit integers (`TimeoutSeconds`, counts) are JSON numbers. The schema's
bounds keep the values a request can carry exact in a JavaScript number.

## Responses

Every response is `{"result": …}` or `{"error": {"code", "message", "field"?}}`
(`ResponseBody`), with `RunnerQ-Storage-Version: 1`. An operation without a
result returns `{"result":null}`; reads of a missing record (`GetActivity`,
`GetResult`) return a null result, not an error.

Clients decode the error code, not the HTTP status. The codes are the storage
error kinds (`ErrorCode`, constants.json `storage_error_kind`); an unknown code
is a configuration error. `field` names the input of an `invalid_argument` or
`unsupported` error. Internal errors are logged by the server and redacted.

## Limits

- Request bodies are at most 4 MiB; the Go adapter reads responses up to 16 MiB.
- Unknown argument fields and trailing JSON are rejected.
- The schema's bounds are enforced: worker tokens 1 to 512 bytes, page and
  batch sizes up to 1,000, durations up to 24 hours, serialization names up to
  64 bytes, an activity's bounds (`QueuedActivity`).
- A zero-limit batch claim returns at once with no claims.
- Requests are bounded at 30 seconds. Dequeue waits are capped at 25 seconds
  (`x-server-clamp`) and stay cancellable.

## Operations

The operations cover the Go storage interfaces named by each operation's
`x-go-interface`, including the reads the storage conformance suite checks
through (`storagetest.Reader`). Capabilities with no remote work
(`SchedulesNatively`, `MaintenanceManaged`) are answered by the adapter.

`WaitForResult` (`{"activityID": …}`) blocks for up to 25 seconds and returns
null when there is no result yet. The Go adapter repeats it, as a read, until
the caller gives up.

`CleanupExpired` honours the policy it is given. The data plane's own
maintenance uses the store's retention instead, and current SDK workers don't
call it.

## Durability and retries

Each operation is one transaction on the data plane, as on a worker's own
Postgres; no atomic operation is split across requests. Execution tokens fence
acknowledgements, checkpoints, dependent waits and spawns.

A lost response is an unknown outcome. Adapters don't blindly retry writes or
claims. Identical acknowledgements and checkpoint writes reconcile against
their durable identities. Signals are not idempotent, so they must not be
replayed as if they were reads. Producers reconcile an enqueue with a stable
idempotency key and return-existing behaviour. An abandoned claim is recovered
by the data plane's reaper after its lease expires.

## Worker reports

Each engine using the adapter reports itself for RunnerQ Cloud's Fleet:
`PUT /v1/executors/{id}`, with the store key and version header, sending an
`ExecutorReport` (`protocol/conductor`) every 10 seconds and, at most every 2 seconds, when it changes.
On a clean stop it sends a final report and then `DELETE /v1/executors/{id}`.

The report's single queue is recorded like a first use. Reports are at most
64 KiB, and fields the data plane doesn't know are dropped. Workers not seen
for a week are forgotten.

## Versioning

New operations and new fields are additive within version 1. Removing or
renaming either, or changing a field's type or meaning, needs a new
`RunnerQ-Storage-Version`.
