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
make bench_compare COMPARE_ARGS='--reference /tmp/reference.json --candidate /tmp/candidate.json --workload-root /absolute/workload --reference-exit 1 --candidate-exit 1 --out dist/bench/comparison'
```

The comparator normalizes the workload root, sorts diagnostics and report
metadata, compares exit codes, and writes both normalized outputs plus a JSON
summary. Generate inputs sequentially with isolated caches and the baseline
harness resource limits.

## Benchmark one linter: with a local version

```bash
make bench_local LINTER=gosec VERSION=v1.59.0
```

## Benchmark one linter: between 2 existing versions

```bash
make bench_version LINTER=gosec VERSION_OLD=v1.58.1 VERSION_NEW=v1.59.0 
```
