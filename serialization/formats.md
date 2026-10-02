# Serialization formats

Every stored payload, checkpoint and result carries a serialization id next to
its data (`runnerq_inputs.serialization`, `runnerq_results.serialization`, and
`Serialization` on the storage protocol).

| Id | Data | Written by | Read by |
| --- | --- | --- | --- |
| `json-v1` (or empty) | Plain JSON | Go, and TypeScript in portable mode | Every SDK |
| `superjson-v1` | A SuperJSON `{json, meta}` envelope | TypeScript in native mode (the default) | TypeScript. Go claims skip work with this input, so it stays queued for a TypeScript worker. |

Outside the SDK that wrote it (in queries and the console), a value shows as
`PlainJSON` (`vectors/plain_json.json`). For `superjson-v1` that is the
envelope's `json` member: JSON-safe, without type metadata.

A format id never changes meaning; a new encoding gets a new id.
