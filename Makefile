.DEFAULT_GOAL = test
.PHONY: FORCE

# enable consistent Go 1.12/1.13 GOPROXY behavior.
export GOPROXY = https://proxy.golang.org

BINARY = golangci-lint
ifeq ($(OS),Windows_NT)
	BINARY := $(BINARY).exe
endif

# Build

build: $(BINARY)
.PHONY: build

build_race:
	go build -race -o $(BINARY) ./cmd/golangci-lint
.PHONY: build_race

clean:
	rm -f $(BINARY)
	rm -f test/path
	rm -f tools/Dracula.itermcolors
	rm -f tools/svg-term
	rm -rf tools/node_modules
.PHONY: clean

# Test
test: export GOLANGCI_LINT_INSTALLED = true
test: CGO_ENABLED=1
test: build
	GL_TEST_RUN=1 ./$(BINARY) run -v
	GL_TEST_RUN=1 go test -v -parallel 2 ./...
.PHONY: test

test_race: build_race
	GL_TEST_RUN=1 ./$(BINARY) run -v --timeout=5m
.PHONY: test_race

# ex: T=output.go make test_integration
# the value of `T` is the name of a file from `test/testdata`
test_integration:
	GL_TEST_RUN=1 go test -v ./test -count 1 -run TestSourcesFromTestdata/$T
.PHONY: test_integration

# ex: T=multiple-issues-fix.go make test_integration_fix
# the value of `T` is the name of a file from `test/testdata/fix`
test_integration_fix: build
	GL_TEST_RUN=1 go test -v ./test -count 1 -run TestFix/$T
.PHONY: test_integration_fix

# Rust supervisor

rust_fmt:
	cd rust && cargo fmt --all -- --check
.PHONY: rust_fmt

rust_lint:
	cd rust && cargo clippy --workspace --all-targets --locked -- -D warnings
.PHONY: rust_lint

rust_test:
	cd rust && cargo test --workspace --all-targets --locked
.PHONY: rust_test

rust_build:
	cd rust && cargo build --release --locked --bin golangci-supervisor
.PHONY: rust_build

rust_check: rust_fmt rust_lint rust_test rust_build
.PHONY: rust_check

# Maintenance

fast_generate: assets/github-action-config.json
.PHONY: fast_generate

fast_check_generated:
	$(MAKE) --always-make fast_generate
	git checkout -- go.mod go.sum # can differ between go1.16 and go1.17
	git diff --exit-code # check no changes

# Migration

clone_config:
	go run ./pkg/commands/internal/migrate/cloner/

# Benchmark

# Benchmark with a local version
# LINTER=gosec VERSION=v1.59.0 make bench_local
bench_local: hyperfine
	@:$(call check_defined, LINTER VERSION, 'missing parameter(s)')
	@./scripts/bench/bench_local.sh $(LINTER) $(VERSION)
.PHONY: bench_local

# Benchmark between 2 existing versions
# make bench_version LINTER=gosec VERSION_OLD=v1.58.2 VERSION_NEW=v1.59.0
bench_version: hyperfine
	@:$(call check_defined, LINTER VERSION_OLD VERSION_NEW, 'missing parameter(s)')
	@./scripts/bench/bench_version.sh $(LINTER) $(VERSION_OLD) $(VERSION_NEW)
.PHONY: bench_version

BENCH_FORK_BIN ?= $(CURDIR)/dist/bench/bin/fork
BENCH_PAIR_DIR ?= $(CURDIR)/dist/bench/bin
BENCH_CANDIDATE_REF ?= HEAD
BENCH_UPSTREAM_REF ?= upstream/main
BENCH_COMPAT_MANIFEST ?= scripts/bench/compatibility.json

bench_baseline:
	mkdir -p $(dir $(BENCH_FORK_BIN))
	go build -trimpath -ldflags '-s -w' -o $(BENCH_FORK_BIN) ./cmd/golangci-lint
	go run ./scripts/bench/baseline \
		--manifest scripts/bench/baseline.json \
		--fork-bin $(BENCH_FORK_BIN) \
		$(if $(UPSTREAM_BIN),--upstream-bin $(UPSTREAM_BIN)) \
		$(BENCH_ARGS)
.PHONY: bench_baseline

bench_pair:
	GOMAXPROCS=2 GOFLAGS=-p=2 GOMEMLIMIT=1024MiB go run ./scripts/bench/build_pair \
		--candidate-ref $(BENCH_CANDIDATE_REF) \
		--upstream-ref $(BENCH_UPSTREAM_REF) \
		--out-dir $(BENCH_PAIR_DIR) \
		--build-timeout 5m \
		--max-memory-mib 1024 \
		--go-max-procs 2 \
		--nice 10
.PHONY: bench_pair

bench_compat:
	@:$(call check_defined, BENCH_ARGS, 'explicit workload and concurrency filters required')
	$(MAKE) bench_pair
	GOMAXPROCS=2 GOFLAGS=-p=2 GOMEMLIMIT=1024MiB go run ./scripts/bench/baseline \
		--manifest scripts/bench/baseline.json \
		--fork-bin $(BENCH_PAIR_DIR)/fork$(suffix $(BINARY)) \
		--upstream-bin $(BENCH_PAIR_DIR)/upstream$(suffix $(BINARY)) \
		--compatibility \
		--run-timeout 2m \
		--max-rss-mib 1024 \
		--go-max-procs 2 \
		--nice 10 \
		$(BENCH_ARGS)
.PHONY: bench_compat

bench_compat_corpus:
	$(MAKE) bench_pair
	GOMAXPROCS=2 GOFLAGS=-p=2 GOMEMLIMIT=1024MiB go run ./scripts/bench/baseline \
		--manifest $(BENCH_COMPAT_MANIFEST) \
		--fork-bin $(BENCH_PAIR_DIR)/fork$(suffix $(BINARY)) \
		--upstream-bin $(BENCH_PAIR_DIR)/upstream$(suffix $(BINARY)) \
		--compatibility \
		--workload directive-corpus \
		--concurrency 1 \
		--run-timeout 2m \
		--max-rss-mib 1024 \
		--go-max-procs 2 \
		--nice 10 $(if $(BENCH_CORPUS_SCENARIO),--scenario $(BENCH_CORPUS_SCENARIO))
.PHONY: bench_compat_corpus

bench_compare:
	go run ./scripts/bench/compare $(COMPARE_ARGS)
.PHONY: bench_compare

hyperfine:
	@which hyperfine > /dev/null || (echo "Please install hyperfine https://github.com/sharkdp/hyperfine#installation" && exit 1)
.PHONY: hyperfine

# Non-PHONY targets (real files)

$(BINARY): FORCE
	go build -o $@ ./cmd/golangci-lint

assets/github-action-config.json: FORCE $(BINARY)
	# go run ./scripts/gen_github_action_config/main.go $@
	cd ./scripts/gen_github_action_config/; go run . ../../$@

go.mod: FORCE
	go mod tidy
	go mod verify
go.sum: go.mod

# Documentation

docs_serve: website_expand_templates
	@make -C ./docs serve
.PHONY: docs_serve

docs_clean:
	@make -C ./docs clean
.PHONY: docs_clean

docs_build: website_copy_install_sh website_copy_jsonschema website_expand_templates
	@make -C ./docs build
.PHONY: docs_build

docs/static/demo.gif: FORCE
	vhs docs/golangci-lint.tape

website_copy_jsonschema:
	 go run ./scripts/website/copy_jsonschema/
.PHONY: website_copy_jsonschema

website_copy_install_sh:
	 cp install.sh ./docs/static/
.PHONY: website_copy_install_sh

website_expand_templates:
	go run ./scripts/website/expand_templates/
.PHONY: website_expand_templates

website_dump_info:
	go run ./scripts/website/dump_info/
.PHONY: website_dump_info

# Functions

# Check that given variables are set and all have non-empty values,
# die with an error otherwise.
#
# Params:
#   1. Variable name(s) to test.
#   2. (optional) Error message to print.
#
# https://stackoverflow.com/a/10858332/8228109
check_defined = \
    $(strip $(foreach 1,$1, \
        $(call __check_defined,$1,$(strip $(value 2)))))
__check_defined = \
    $(if $(value $1),, \
      $(error Undefined $1$(if $2, ($2))))
