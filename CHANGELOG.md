# Changelog

## 0.2.0

Phase 2: the failure path and the query mappings.

- Vectors: `attempts_remain` (the retry decision), `retry_delay` (backoff),
  `canonical_status`, `canonical_event`, `internal_events`.
- Constant `default_max_retry_delay_seconds` (3600), and the `int` constant
  type: a plain number in TypeScript.

## 0.1.0

First release (phase 1).

- `constants/constants.json`: schema advisory lock key, notification channel
  prefixes, business and step key prefixes, serialization ids, storage error
  kinds.
- Vectors: `checkpoint_id`, `business_key`, `application_key`, `step_key`,
  `serialization/plain_json`.
- `tools/gen`: Go and TypeScript constants.
- `verify`: reference checks for every vector.
