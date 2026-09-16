# Worker protocol version 1

The experimental Rust controller and Go analysis worker exchange one UTF-8
JSON object per line. Encoders terminate every record with `\n`; decoders also
accept a final unterminated record and `\r\n`. The JSON object, excluding its
line terminator, is limited to 1 MiB.
Object and array nesting is limited to 64 containers per record.

Every envelope contains `protocol_version`, `kind`, and an object `payload`.
Run-scoped messages contain a non-empty `request_id`. Worker-to-controller
events contain a positive, connection-wide, monotonically increasing
`sequence`. Unknown envelope fields, versions, and kinds are rejected. Unknown
payload fields are retained by decode/encode round trips so version 1 can grow
additively. Duplicate object keys and explicitly empty optional identifiers are
rejected. Envelope field names are case-sensitive. Strings must contain valid
Unicode scalar values; unpaired UTF-16 surrogate escapes are rejected.
Canonical output escapes U+2028 and U+2029 as `\u2028` and `\u2029`.

Controller messages are `hello`, `run`, `cancel`, and `shutdown`. Worker events
are `ready`, `lifecycle`, `diagnostic`, `complete`, `shutdown_ack`, and `error`.
The worker emits `shutdown_ack` only after consuming `shutdown`. Paths are
UTF-8 strings and durations are integer nanoseconds. The protocol deliberately
omits live AST, type, package, analyzer, and suggested-fix objects.

Stable decode error classes are `too_large`, `malformed_json`,
`unsupported_version`, `unknown_kind`, `invalid_envelope`, `invalid_payload`,
and `invalid_sequence`. Details are diagnostic only.

`v1/session.jsonl` and `v1/error.jsonl` are canonical valid streams.
`v1/normalization.json` covers accepted non-canonical input, and
`v1/invalid.json` is the canonical rejection corpus used by both languages.
