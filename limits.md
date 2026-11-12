# Limits

A limit caps how much of one activity type a queue runs: how many at once
across every worker (`max_concurrent`), how many are claimed per period
(a rate), or both. It lives in `runnerq_limits` (migration `0005_limits`),
keyed by queue and activity type, so every worker of the queue, in any
language, enforces the same value. A type without a row is unlimited.

A worker can also cap a type for itself (Go `MaxConcurrentPerWorker`, TypeScript
`maxConcurrentPerWorker`). That cap is the worker's own: nothing is stored and
no other worker sees it.

## Columns

| Column | Meaning |
| --- | --- |
| `queue_name`, `activity_type` | The key. |
| `max_concurrent` | At most this many activities of the type are running at once in the queue; 1 or more, or null for no cap. |
| `rate_tokens`, `rate_per_seconds` | At most `rate_tokens` claims in a burst, refilled at `rate_tokens` per `rate_per_seconds` seconds. `rate_tokens` is 1 or more, `rate_per_seconds` is above 0; both are null for no rate limit. |
| `tokens`, `refilled_at` | The rate bucket: its level, and when it was last brought up to date. 0 and the row's creation when there is no rate. |
| `updated_at` | When `max_concurrent`, `rate_tokens` or `rate_per_seconds` last changed. |

A row has `max_concurrent`, a rate, or both. Removing both is deleting the row.

## What counts

`max_concurrent` counts the type's **running** activities in the queue: rows
with status `processing` (canonical `running`). Nothing else counts:

- **Waiting** activities (parked on a sleep, a signal or a child) hold no worker
  and don't count. One that wakes counts again once it is claimed, and it waits
  for room like any other.
- **Retrying** activities, waiting out their backoff, don't count until they are
  claimed again.
- Pending and scheduled ones don't count.
- A running activity whose worker died counts until its lease expires and the
  reaper requeues it. The count is read from the rows on every claim, so it
  never drifts.

A rate limit counts **claims**: each claim of the type takes one token, whatever
the attempt. A retry, a requeue after a lost lease and a wake after a park each
take one when they are claimed.

## Claiming under a limit

A claim of a set of types splits it in two:

- **Unlimited types** are claimed exactly as without limits: one statement, no
  extra work.
- **Limited types** are claimed in one transaction:
  1. Lock the types' `runnerq_limits` rows (`SELECT … FOR UPDATE`, ordered by
     `activity_type` so concurrent claimers never deadlock).
  2. In a **later statement** than the lock (so its snapshot sees every claim
     committed before the lock was granted), for each locked type:
     - running = the type's `processing` rows in the queue;
     - available tokens = `LEAST(rate_tokens, tokens + elapsed * rate_tokens /
       rate_per_seconds)`, `elapsed` being the seconds from `refilled_at` to
       `clock_timestamp()` (never negative);
     - allowance = the smallest of `max_concurrent - running` (when set),
       `floor(available tokens)` (when there is a rate) and the claim's limit.
  3. Claim up to each type's allowance, with the eligibility and order of an
     ordinary claim, and no more than the claim's limit across the types.
  4. For each rate-limited type that claimed `n > 0`: `tokens = available - n`,
     `refilled_at` = the time `elapsed` was measured at. A type that claimed
     nothing keeps its row as it was; the next claim's refill covers the gap.

  Claimers of a limited type wait for each other on its row, so a count can't
  be raced: two workers never both see "9 of 10". They wait only while a claim
  can take something: an implementation may first check, without the lock,
  whether a type has claimable work, room under `max_concurrent` and a token,
  and skip it if not. The check under the lock decides.

The limited part is claimed first, then the unlimited part with what is left of
the claim's limit; the claims are returned in claim order. Which types count as
limited is read from `runnerq_limits`: an implementation may cache the set for
at most `limits_refresh_ms` (1000). A limit **added** to an unlimited type
therefore binds every worker within a second; a limit **changed** or
**removed** binds the next claim, since the claim reads the row under its lock
(a type whose row has gone is claimed as unlimited).

## Rate tokens

The bucket holds at most `rate_tokens`. It starts full and refills continuously
by the database clock: `rate_tokens` per `rate_per_seconds`, fractions kept.
Over any window of `w` seconds a type is claimed at most `rate_tokens + w *
rate_tokens / rate_per_seconds` times: the burst plus the refill. `Rate 5 per
second` allows a burst of 5, then one claim every 200 ms on average.

Setting a limit:

- **Creates** the row with a full bucket (`tokens = rate_tokens`).
- **Keeps** the bucket when the rate is unchanged, so restarting a fleet that
  declares the same limit doesn't refill it. An unchanged definition writes
  nothing, `updated_at` included.
- **Settles** the bucket when the rate changes: the old rate's refill is applied
  up to now, then the level is capped at the new `rate_tokens` (a new rate on a
  type that had none starts full). `refilled_at` becomes now.
- **Removing** the rate sets `tokens` to 0.

## Changing a limit

Limits are declared where a handler is registered and upserted when the worker
starts, and can be set, read, listed and deleted at runtime through the client.
The last write wins: a worker that starts with a declared limit overwrites a
value set at runtime, and a worker that declares none leaves the stored one
alone.

A lowered limit never preempts: activities already running finish, and the type
claims nothing until the running count drops below the new cap. A deleted limit
lets the next claim take the type freely.

## Ordering

A limit only holds work back; it never reorders it. Within a type, claims follow
priority, then attempts, then due time, as without limits. A held-back
higher-priority activity of a limited type doesn't stop lower-priority work of
other types. A delayed or retrying activity becomes claimable at its
`scheduled_at`, and is then claimed when its type has room, which may be later.

## Waking claimers

New work is announced on the queue's work channel, but a slot freed on another
worker or a refilled token is not. When a limit held back work that was
otherwise claimable, a worker claims again within `limit_probe_ms` (200)
instead of waiting for a work notice.

## Per-worker caps

`MaxConcurrentPerWorker(n)` keeps at most `n` activities of the type running on
one worker. The worker leaves a type at its cap out of its claims and never
claims more of it than it has room for. It applies alongside the queue's limits:
a type with both runs at most `max_concurrent` across the fleet and `n` on each
worker.

## Older workers

A worker that predates limits ignores `runnerq_limits` and claims the type
freely. Upgrade every worker serving a limited type before relying on its limit.
