# Schedules

A schedule enqueues an activity on a cron expression. It lives in
`runnerq_schedules` (migration `0003_schedules`), keyed by queue and name, so
every worker of a queue, in any language, sees the same schedules and fires
each tick once.

## Columns

| Column | Meaning |
| --- | --- |
| `queue_name`, `name` | The key. A name is any non-empty text. |
| `cron` | The expression, in the dialect below. |
| `timezone` | An IANA zone name (`UTC`, `America/New_York`). Not empty, not `Local`. |
| `activity_type` | The type each fire enqueues. |
| `payload`, `serialization` | The fired activity's input, stored as in `runnerq_inputs`. |
| `options` | The fired activity's execution options (below). |
| `catch_up`, `catch_up_limit` | What to do with ticks that came due while no worker fired them. The limit is 1 to 1000. |
| `paused` | A paused schedule fires nothing. |
| `next_fire_at` | The earliest tick not yet handled. Always a fire time of the cron. |
| `last_fire_at` | The latest tick fired, or null. Triggers don't count. |
| `created_at`, `updated_at` | When the row was created and its definition last changed. |

`options` is a JSON object; an absent key takes the default:

| Key | Default | Becomes |
| --- | --- | --- |
| `priority` | 2 | `runnerq_activities.priority` (1 to 4) |
| `max_retries` | 0 (unlimited) | `max_retries`, the attempt limit |
| `timeout_seconds` | 300 | `timeout_seconds` |
| `retry_delay_seconds` | 1 | `retry_delay_seconds` |
| `max_retry_delay_seconds` | 0 (the default cap) | `max_retry_delay_seconds` |
| `metadata` | `{}` | `metadata` (string values) |
| `code_version` | absent (unstamped) | `code_version`, the version the fire is pinned to ([versions.md](versions.md)) |

Options are resolved when a schedule is upserted: an SDK writes every key,
filling what the application left out from its own activity defaults, and
`code_version` when the upserting process has a version. Firing
reads only the row, so a tick's activity is the same whichever SDK fires it;
the defaults above apply only to keys a row lacks (written by other means),
and every implementation applies the same ones.

## Cron dialect

Five fields, `minute hour day-of-month month day-of-week`, or six with
`second` first. Fields are separated by spaces or tabs.

| Field | Values |
| --- | --- |
| second | 0-59 |
| minute | 0-59 |
| hour | 0-23 |
| day of month | 1-31 |
| month | 1-12 or `JAN`-`DEC` |
| day of week | 0-7 or `SUN`-`SAT`; 0 and 7 are Sunday |

A field is a comma-separated list of items. An item is `*`, a value, a range
`a-b` (`a` ≤ `b`), or either of those or a single value followed by `/n`
(`n` ≥ 1), meaning every `n`th value from the start (to the field's end for a
single value: `5/20` is 5, 25, 45). Values are at most two digits. Names are
three letters, in any case, as values or range ends.

Macros, in any case: `@yearly` and `@annually` (`0 0 1 1 *`), `@monthly`
(`0 0 1 * *`), `@weekly` (`0 0 * * 0`), `@daily` and `@midnight`
(`0 0 * * *`), `@hourly` (`0 * * * *`).

Not supported: `L`, `W`, `#`, `?`, years, `@reboot`, `@every`.

**Days.** As in Vixie cron: when the day-of-month and day-of-week fields both
begin with something other than `*`, a day matches if either matches.
Otherwise it must match both (`*/2` counts as beginning with `*`).

An expression that can match no date (`0 0 30 2 *`) is rejected.

## Fire times

A schedule's fire times are instants, to the second. The expression matches
wall-clock times in the schedule's zone. Where the zone's offset is constant,
an instant is a fire time when its wall-clock time matches. Daylight-saving
changes follow one rule, which depends on whether the **hour field begins
with `*`** (`*`, `*/2`):

- **Fixed hour** (`30 2 * * *`, `0 9 * * 1-5`): the schedule fires once per
  matching wall-clock time. When the clock springs forward over matching wall
  times, it fires once at the instant the gap ends (03:00 EDT for 02:30 on
  the day New York skips 02:00-02:59). When the clock falls back, it fires in
  the first pass through the repeated wall times only.
- **Any hour** (`0 * * * *`, `*/15 * * * *`): the schedule follows real time.
  Wall times the clock skips never fire; wall times it repeats fire in both
  passes. An hourly schedule fires every real hour through either change.

`vectors/cron_next_fire.json` pins the dialect and these rules, and
`verify/cron_test.go` is a reference implementation. A conforming
implementation may assume a zone's offset changes at most once in any 7 days.
`next(from)` searches 50 years ahead.

## Firing

A worker's maintenance loop fires its queue's due schedules, about every
second, using the database clock (`NOW()`), never its own. Each due schedule
is fired in **one transaction**:

1. Claim a due row:
   ```sql
   SELECT ... FROM runnerq_schedules
   WHERE queue_name = $1 AND NOT paused AND next_fire_at <= NOW()
   ORDER BY next_fire_at LIMIT 1
   FOR UPDATE SKIP LOCKED
   ```
   (`idx_runnerq_schedules_due` serves it.) A row another worker holds is
   skipped, not waited for.
2. Compute what to fire (`vectors/schedule_fire.json`):
   - The **due ticks** are the fire times `t` with `next_fire_at <= t <= NOW()`.
   - `one` (the default) fires the latest due tick.
   - `skip` fires the latest due tick only if it is less than
     `schedule_catch_up_grace_seconds` (60) old; otherwise nothing.
   - `all` fires the latest `catch_up_limit` due ticks (default
     `schedule_catch_up_limit_default`, 100), oldest first.
3. Enqueue one activity per fire time `t`: a root activity of
   `activity_type`, with the payload and options, and the idempotency key
   `schedule:<name>:<t>`, `t` in RFC 3339 UTC to the second
   (`schedule:nightly:2026-03-08T07:00:00Z`). The key is stored like any
   application key: `vectors/business_key.json` with the activity type. When
   the key already exists the tick was fired and nothing is enqueued.
4. Set `next_fire_at` to the first fire time after `NOW()`, and
   `last_fire_at` to the latest fire time enqueued, if any.
5. Commit, then announce the new work as any enqueue does, and claim the
   next due row, up to a batch limit.

The claim makes racing workers take different rows. The single transaction
means a crash leaves either the enqueue and the advance or neither. The key
makes a lost advance harmless anyway: firing the same tick again enqueues
nothing.

A row that fails (an expression or zone this implementation rejects, perhaps
written by a newer SDK, or a failed write) is rolled back and reported; the
loop leaves it out for the rest of the batch and fires the others.

## Operations

- **Upsert** by name. A new row starts unpaused with `next_fire_at` the first
  fire time after now. Updating a row replaces its definition and keeps
  `paused` and `last_fire_at`; it keeps `next_fire_at` too unless `cron` or
  `timezone` changed, in which case `next_fire_at` becomes the first fire time
  after now. Upserting an unchanged definition, as every worker does at
  start, changes nothing.
- **Pause** sets `paused`. **Resume** clears it and sets `next_fire_at` to the
  first fire time after now: ticks missed while paused are not caught up.
  Resuming a schedule that isn't paused changes nothing.
- **Trigger** enqueues the activity now, as a fire at `NOW()` truncated to the
  second, whether or not the schedule is paused, and changes nothing on the
  row. Two triggers in the same second, or a trigger at a tick's second, make
  one activity; the second returns the first's id.
- **Delete** removes the row. Activities already fired are unaffected.
- **List** and **get** read rows.

Pause, resume, trigger and delete of an unknown name fail with `not_found`.

## Schedules declared in code

An SDK may let an application declare schedules on its worker; the worker
upserts them when it starts. Workers that declare the same schedule
differently overwrite each other's definition on each start, so roll such a
change out like a schema change.
