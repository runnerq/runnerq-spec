# runnerq-spec

The language-neutral contract every RunnerQ implementation agrees on. It holds:

- the constants they share;
- golden vectors for the logic each SDK implements on its own;
- the Postgres schema;
- the wire protocols (later phase);
- a cross-language conformance suite (later phase).

There is no runtime code here, only definitions, a generator and test data.

Consumers: [runnerq-go](https://github.com/runnerq/runnerq-go),
[runnerq-ts](https://github.com/runnerq/runnerq-ts),
[cloud-storage-go](https://github.com/runnerq/cloud-storage-go),
[cloud-storage-ts](https://github.com/runnerq/cloud-storage-ts) and RunnerQ Cloud.

## Why

Go and TypeScript implement RunnerQ separately against one Postgres schema and
two wire protocols. Until now, parity was kept by hand:

- **Schema:** Go owns the DDL, and TypeScript re-declares it and validates the catalog.
- **Storage protocol:** it is regex-generated from Go source and mirrored by hand in TypeScript.
- **Conductor protocol:** there are three hand copies of a prose spec.
- **Pure logic:** functions like checkpoint IDs and idempotency keys are written twice and pinned by a few golden values.

This repo makes each of those a single source. The idea follows Temporal's
`coresdk` protos and `features` repo, without shipping a shared native core.

## Layout

```
runnerq-spec/
├── VERSION                     spec release (semver)
├── VERSIONING.md               compatibility rules
├── CHANGELOG.md
├── constants/
│   └── constants.json          lock key, channel and key prefixes,
│                               serialization ids, storage error kinds
├── vectors/                    golden inputs → outputs for pure logic
│   ├── checkpoint_id.json
│   ├── business_key.json
│   ├── application_key.json
│   ├── step_key.json
│   ├── attempts_remain.json    retry decision
│   ├── retry_delay.json        backoff
│   ├── canonical_status.json
│   ├── canonical_event.json
│   ├── internal_events.json
│   ├── index_definition.json   catalog normalization
│   └── column_default.json
├── schema/
│   └── postgres/               migrations, concurrent indexes, catalog
│                               (see its README)
├── serialization/
│   ├── formats.md              json-v1, superjson-v1
│   └── vectors/
│       └── plain_json.json
├── verify/                     reference implementation; checks every vector
└── tools/
    ├── gen/                    constants and schema → Go / TypeScript source
    └── catalog/                schema → catalog.json, from a real Postgres
```

Later phases add these directories:
- `schema/postgres/functions/`, possibly, for SQL state-transition functions.
- `protocol/` for the storage, executor-report and conductor protocols as JSON Schema.
- `conformance/` for data-driven scenarios and the mixed Go/TS fleet test.

## Vectors

Every vector file has the same shape:

```json
{
  "description": "The rule, precisely enough to implement from.",
  "cases": [
    { "name": "unique case name", "input": { ... }, "output": ... }
  ]
}
```

Rules:
- Outputs compare as JSON values.
- `verify/` implements each description from scratch using only Go's standard library, and fails if any output disagrees.
- The UUID and base64 vectors have also been checked against Python's `uuid.uuid5` and `base64`.

To add a case:
1. Add it with `"output": null`.
2. Run `go test ./verify -update` to fill in the output.
3. Check the filled-in value by some independent means before committing.

## Generated code

```sh
go run ./tools/gen -lang go -pkg spec -out <file.go>                # constants
go run ./tools/gen -lang ts -out <file.ts>
go run ./tools/gen -lang go -pkg spec -part schema -out <file.go>   # Postgres schema
go run ./tools/gen -lang ts -part schema -out <file.ts>
```

Go names are PascalCase with Go initialisms (`SerializationJSON`); TypeScript
names are camelCase (`serializationJson`). Enums become numbered constants in Go
and an `as const` object in TypeScript. 64-bit integers are decimal strings in
TypeScript.

## How consumers use it

1. Add this repo as a git submodule at `spec/`, pinned to a release tag.
2. Generate constants from the submodule into the consumer, and commit the output.
   - runnerq-go: `go generate ./internal/spec`
   - runnerq-ts: `npm run spec:gen`
   Because the output is committed, `go get` and npm-from-git users never need the submodule.
3. Tests read the vectors straight from `spec/`.
4. CI checks out submodules, regenerates the constants, and fails on any diff.

Bumping the spec in a consumer is one commit: the submodule pointer, the regenerated files and any code changes.

## Format choices

**Everything is JSON.** The generator and the verifier need only Go's standard
library. Both wire protocols are JSON, and protocol v1 must stay byte-identical:
the storage protocol uses Go's default JSON, with capitalized field names and
durations in nanoseconds. So the protocols will be described with JSON Schema.
Protobuf's JSON mapping would rename fields and re-encode durations, which would
force a v2.

## Rollout

| Phase | Scope | Status |
| --- | --- | --- |
| 1 | Constants, pure-logic vectors, generator; both SDKs use the constants and test against the vectors | v0.1.0 |
| 2 | Extract backoff and the retry decision into pure functions; vectors for them and the status and event mappings | v0.2.0 |
| 3 | Schema: migrations, concurrent indexes and catalog; both SDKs migrate and validate from it | v0.3.0 |
| 4 | Protocols: storage, executor reports, conductor; generated types replace `generate.py` and the hand copies | |
| 5 | Conformance scenarios, Go and TS drivers, mixed-fleet job, storaged protocol replay | |
| 6 | Decide on SQL functions for state transitions | |

## Development

```sh
go vet ./... && go test ./...
```

CI also checks that the generator's output for both languages builds and is formatted.
