# Rust supervisor

`golangci-supervisor` is the first Rust extraction boundary in this fork. It
runs one Go `golangci-lint` process without changing its arguments or standard
streams, then owns timeout, cancellation, process-tree cleanup, and optional
RSS enforcement.

The Go executable remains the default entry point and rollback path. The
supervisor is opt-in:

```sh
cd rust
mise install
cargo build --release --locked --bin golangci-supervisor
GOLANGCI_SUPERVISOR_TARGET=/path/to/golangci-lint \
  target/release/golangci-supervisor run ./...
```

## Configuration

The supervisor consumes these environment variables and removes them from the
child environment:

| Variable | Meaning |
| --- | --- |
| `GOLANGCI_SUPERVISOR_TARGET` | Required path or executable name for the Go CLI. |
| `GOLANGCI_SUPERVISOR_TIMEOUT_MS` | Optional wall timeout; `0` disables it. |
| `GOLANGCI_SUPERVISOR_MAX_RSS_BYTES` | Optional process-tree RSS limit; `0` disables it. |
| `GOLANGCI_SUPERVISOR_REPORT` | Optional path for an atomic JSON outcome report. |
| `GOLANGCI_SUPERVISOR_POLL_MS` | Lifecycle polling interval; defaults to `10`. |

The versioned report records the termination class, child exit or signal,
elapsed time, peak tree RSS, cancellation latency, cleanup result, and wrapper
error. Standard output and standard error remain the child's streams.

The Go child can independently emit detailed phase metrics by setting
`GOLANGCI_LIFECYCLE_REPORT` to a JSON output path. The supervisor passes this
variable through unchanged. Its versioned report covers package loading,
analysis graph size and cache use, each optimized linter invocation, each
configured Go-analysis linter and analyzer, result processing, and the final Go
exit state. Report-write failures warn without changing the Go exit code.

Normal child exits are preserved. Supervisor-owned exits are `124` for timeout,
`125` for RSS limit, `126` for supervisor failure, `127` for child startup
failure, and `130` for cancellation. Use the report to distinguish a child
that independently returns one of those codes.

## Development

Run `make rust_check` from the repository root. The pinned toolchain and lock
file make local and CI builds reproducible.
