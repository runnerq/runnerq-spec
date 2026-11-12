# Observability

What a RunnerQ SDK reports about itself, named once so Go and TypeScript
dashboards, traces and logs read the same.

| File | What it is |
| --- | --- |
| `metrics.json` | Every metric: name, kind (counter, histogram, gauge), unit and labels. |
| `tracing.json` | Span names, kinds, attributes and outcomes, and the metadata key the trace context is stored under. |
| `../vectors/traceparent.json` | Which traceparent an enqueue stores. |

`tools/gen -part observability` writes the names as Go or TypeScript.

## Metrics

An SDK reports each metric with the labels `metrics.json` lists for it, to a
labelled sink the application provides. Durations are seconds. The gauges are
sampled on a timer, off by default, whose period each worker jitters by up to
a fifth so a fleet doesn't query in step:

- `queue_depth` comes from the storage's aggregate query (count, grouped by
  activity type and status, statuses `pending`, `scheduled`, `running`,
  `waiting` and `dead_letter`), and needs a backend that answers queries.
- `queue_oldest_pending_age` comes from the oldest pending activity by
  creation time: now minus the later of its creation and scheduled times.
- `activities_in_flight` is the worker's own count, for every type it serves.

## Tracing

The core of an SDK depends on no tracing library. It calls a tracer
interface with the span names and attributes of `tracing.json`, and reads the
current span back as a W3C `traceparent`. The optional OpenTelemetry
integrations (`github.com/runnerq/runnerq-go/otel`, `runnerq/otel`) implement
it.

1. **Enqueue.** The SDK starts `runnerq.enqueue` (with a tracer) and stores a
   traceparent in the activity's metadata under `traceparent`, chosen by
   `vectors/traceparent.json`: the caller's own value, else the enqueue
   span's, else the traceparent of the activity whose handler is enqueueing.
   Without a tracer a child still inherits its parent's value, so every
   activity of a tree keeps the root's trace id.
2. **Attempt.** Each attempt's `runnerq.attempt` span has the stored
   traceparent as its parent. Retries, and the attempts that resume after a
   durable sleep or signal wait, are siblings under the enqueue span: one
   trace from enqueue to the last step.
3. **Inside an attempt.** `runnerq.step`, `runnerq.sleep` and
   `runnerq.signal.wait` are children of the attempt span. A child activity's
   enqueue is a child of the attempt or step that spawned it.

Every span ends with `runnerq.outcome` set to one of its outcomes; the tracer
marks it as an error when the SDK passes one.

## Logs

Both SDKs log the same events with the same field names, Go through `slog`
and TypeScript through its `logger` option (by default JSON lines on stderr
with `time`, `level` and `msg`):

| Level | Message | Fields |
| --- | --- | --- |
| info | `Starting worker engine` | `queue`, `max_concurrent_activities` |
| info | `Worker engine stopped` | `queue` |
| info | `Activity completed successfully` | `activity_id`, `activity_type` |
| warn | `Activity requesting retry` | `activity_id`, `activity_type`, `reason` |
| error | `Activity failed` | `activity_id`, `activity_type`, `reason` |
| error | `Activity timed out` | `activity_id`, `activity_type`, `timeout` |
| warn | `Activity execution lost its claim` | `activity_id`, `activity_type` |
| debug | `Activity yielded for durable wait` | `activity_id`, `activity_type`, `step`, `wake_at` |
| error | `Failed to confirm activity completion` | `activity_id`, `activity_type`, `error` |

An SDK may log more; these are the ones operators alert on.
