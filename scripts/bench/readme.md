# Benchmarks

The script use [Hyperfine](https://github.com/sharkdp/hyperfine) to benchmark the command line of golangci-lint.

## Reproducible baseline

Build the fork and run the pinned workload matrix:

```bash
make bench_baseline
```

The default matrix measures cold and warm golangci-lint caches with 1, 2, 4,
and 8-way concurrency. It pins child processes to the manifest's Go toolchain
and keeps the Go build and module caches warm. Results, logs, cloned workloads,
and optional profiles stay under ignored `dist/bench/`.

Every golangci-lint process runs at niceness 10 with `GOMAXPROCS=2`, Go build
parallelism 2, a 2 GiB Go memory limit, a 2 GiB process-tree RSS kill threshold,
and a five-minute hard timeout. The limits are recorded in `metadata.json` and
can be tightened with `--nice`, `--go-max-procs`, `--max-rss-mib`, and
`--run-timeout`.

The large Kuma workload targets a pinned 81-file, 29.7k-line generated API
subset with test analysis disabled. Whole-repository Kuma runs exceed the safe
local resource budget.

Benchmark processes also use golangci-lint's serial-runner lock. They wait
instead of overlapping another local golangci-lint process.

Run a quick smoke benchmark:

```bash
make bench_baseline BENCH_ARGS='--workload small --scenario goanalysis --concurrency 1 --runs 1'
```

The manifest includes focused `govet`, `staticcheck`, `unused`, and
`staticcheck-unused` scenarios. Compare them with the combined `goanalysis`
scenario to isolate fact-cache and shared-IR costs:

```bash
make bench_baseline BENCH_ARGS='--workload large --scenario staticcheck-unused --concurrency 4'
```

Select one module from a multi-module workload with an exact path filter:

```bash
make bench_baseline BENCH_ARGS='--workload multi-module --module scripts/gen_github_action_config --concurrency 1,2,4,8'
```

Capture profiles separately from timing samples:

```bash
make bench_baseline BENCH_ARGS='--profiles'
```

Heap profiles use a 64 KiB allocation sample rate to keep profiling overhead
practical. Profile runs never contribute timing samples.

Compare another binary built from the same source base:

```bash
make bench_baseline UPSTREAM_BIN=/absolute/path/to/upstream/golangci-lint
```

Artifacts:

- `metadata.json`: host, toolchain, binary hashes, workload revisions, config hashes.
- `results.jsonl`: one durable record per completed run.
- `logs/`: verbose golangci-lint output.
- `profiles/`: CPU, heap, and runtime trace captures.

Compare JSON diagnostics from reference and candidate binaries:

```bash
GOMAXPROCS=2 GOFLAGS=-p=2 mise exec go@1.26.0 -- \
  make bench_compat \
  BENCH_ARGS='--workload small --scenario goanalysis --concurrency 1'
```

Compatibility mode requires both binaries plus explicit workload and
concurrency filters. It executes upstream then fork sequentially with isolated
cold caches. The comparator normalizes the workload root, sorts diagnostics
and report metadata, compares exit codes, and writes raw and normalized outputs
plus a JSON summary under `compat/`.

Scenarios marked `mutates` run in separate clean detached worktrees. The
comparator also checks deterministic manifests of added, modified, and deleted
files, including modes, symlinks, and binary content. Matching worktrees are
removed; mismatching worktrees are retained and recorded in `summary.json`.

`bench_compat` builds the candidate from `HEAD` and the reference from its merge
base with `upstream/main`, both in clean detached worktrees. Builds are
sequential and capped at two-way Go parallelism, a 1 GiB Go memory limit,
niceness 10, and five minutes. `dist/bench/bin/pair.json` records resolved refs,
commits, binary hashes, and limits. Override refs with `BENCH_CANDIDATE_REF` and
`BENCH_UPSTREAM_REF`.

Run the pinned directive compatibility corpus:

```bash
GOMAXPROCS=2 GOFLAGS=-p=2 mise exec go@1.26.0 -- make bench_compat_corpus
```

The corpus covers diagnostics, custom configuration, fix mode, compile errors,
cgo, path exclusions, and test inclusion. Each case selects its own working
directory and package inside a pinned checkout. Restrict a smoke run with
`BENCH_CORPUS_SCENARIO=fix-mode`. The target always compares at
concurrency 1 with a two-minute timeout, two Go processes, niceness 10, and a
1 GiB RSS ceiling.

For the multi-module workload, always select a module. Do not run its root above
concurrency 1 under a 1 GiB RSS limit:

```bash
GOMAXPROCS=2 GOFLAGS=-p=2 mise exec go@1.26.0 -- \
  make bench_compat \
  BENCH_ARGS='--workload multi-module --module scripts/gen_github_action_config --scenario goanalysis --concurrency 1'
```

Use `make bench_compare` only to compare JSON files captured separately.

## Benchmark one linter: with a local version

```bash
make bench_local LINTER=gosec VERSION=v1.59.0
```

## Benchmark one linter: between 2 existing versions

```bash
make bench_version LINTER=gosec VERSION_OLD=v1.58.1 VERSION_NEW=v1.59.0 
```
