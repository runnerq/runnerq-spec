# Postgres schema

Every RunnerQ implementation uses these tables, in the connection's current
schema (`search_path`). Several SDK versions and languages may share one
database, so all of them must apply and check the schema the same way.

| File | What it is |
| --- | --- |
| `migrations/NNNN_name.sql` | Append-only migrations. Each one is safe to re-run (`IF NOT EXISTS`, guarded `DO` blocks). |
| `concurrent_indexes.json` | Indexes built with `CREATE INDEX CONCURRENTLY` after the migrations, and the indexes they replace. |
| `retired.json` | Columns and indexes the schema no longer has. |
| `catalog.json` | What all of the above produce, read from Postgres by `tools/catalog`. CI checks it on Postgres 16, 17 and 18. |
| `events.schema.json` | The detail each stored event type carries (`runnerq_events.detail`). The conformance runner checks every event a scenario writes against it. |

## Bringing a database up to date

An implementation that migrates does this:

1. **Check without locks.** If the catalog already matches (every table,
   column and index in `catalog.json` exists, every index is valid, and no
   retired object exists), stop. Even no-op DDL takes table locks, so a
   process starting against a live database must not run it.
2. **Take the advisory lock** `schema_advisory_lock_key` (constants.json) with
   `pg_try_advisory_lock`, polling rather than blocking: a blocked
   `pg_advisory_lock` holds a snapshot that a concurrent index build waits
   on, and Postgres breaks that cycle as a deadlock.
3. **Check again**, since another process may have finished while you waited.
4. **Run every migration**, in order, in one transaction.
5. **Build the concurrent indexes**, in order, each statement on its own
   (outside a transaction). For each: drop it if it exists but is invalid
   (a build killed midway), build it if missing, then drop the index it
   replaces. The table is never without one of the pair.
6. **Drop the retired indexes** (`retired.json`) that still exist, each with
   `DROP INDEX IF EXISTS` on its own.
7. **Release the lock**, even if the caller's context was cancelled.

Schema statements can lose deadlocks against live traffic. They are
idempotent, so retry them.

An implementation that does not migrate a given database must refuse it
while it has a retired column: an older SDK version is still using it. A
retired index only costs writes until it is dropped, so it may be tolerated.

## Validating

Compare the live catalog (`information_schema.columns`, `pg_constraint`,
`pg_index` with `pg_get_indexdef`) against `catalog.json`:

- **Columns:** same `udt_name` and nullability, and defaults equal after
  normalizing as `vectors/column_default.json` describes.
- **Primary keys:** the same columns, in the same order.
- **Indexes:** each one present and valid, with a definition equal after
  normalizing as `vectors/index_definition.json` describes.
- **Retired objects:** absent.

## Changing the schema

1. Add `migrations/NNNN_name.sql`, safe to re-run. Never edit a released
   migration.
2. For an index on a busy table, add it to `concurrent_indexes.json` instead.
   To change one, add a new name that `replaces` the old.
3. Regenerate the catalog: `cd tools/catalog && go run . -dsn ... -write`.
4. List any column a migration drops in `retired.json`, and any index the
   schema stops having that no concurrent index replaces. A migration must not
   create a retired index.

Every index on `runnerq_activities` gets a new entry on every status change
(status is in every partial predicate, so those updates are never HOT). Add one
only for a query that runs often, and check with `EXPLAIN` that the query can
use it: a partial index serves a query only when Postgres can prove the
predicate from the query's own conditions, so name statuses as literals rather
than parameters.

## Column contracts

What the schema can't enforce, every implementation keeps:

- `runnerq_activities.root_activity_id` is the activity's own id for a root
  and its root's id otherwise; `parent_activity_id` is null exactly for roots.
  A tree is `id = $root OR (root_activity_id = $root AND parent_activity_id IS
  NOT NULL)`, which `idx_runnerq_root_children` serves.
- `runnerq_dependencies.producer_activity_id` is null (a checkpoint or signal
  result) or equal to `result_id` (an activity's own result, which pins the
  producer's tree).
- `runnerq_activities.started_at` is when the current or last attempt was
  claimed; an event ending an attempt records it (`events.schema.json`).
