# Versioning

A spec release is a semver tag (`vX.Y.Z`) whose number is in `VERSION`. It stays
`v0.x` until the first SDK release depends on it.

Each contract inside the spec also carries its own version, which is already
present on the wire or in the database. The rules below keep Go and TypeScript
workers of different versions safe on one database.

| Contract | Version carrier | Rule |
| --- | --- | --- |
| Constants | none | A released value never changes. Additions are minor releases. |
| Vectors | none | A released case never changes its output. Changing the rule is a new rule with a new name (e.g. `rq:key:v3:`), and the old vectors stay as long as implementations must read old data. |
| Serialization | format id (`json-v1`, `superjson-v1`) | A format id is frozen once released. A change gets a new id. |
| Postgres schema (phase 3) | migration number | Migrations are append-only. A change of meaning for existing rows needs a migration and a capability, never a silent reinterpretation. |
| Storage protocol (phase 4) | `RunnerQ-Storage-Version` header | New fields and new operations are minor. Renames, removals and type changes need a new protocol version. |
| Conductor protocol (phase 4) | envelope `v` + capabilities | Same as the storage protocol. Capability negotiation gates new message types. |

A spec major release is reserved for removing something a released SDK still
reads.
