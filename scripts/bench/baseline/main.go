package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	iofs "io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/process"

	"github.com/golangci/golangci-lint/v2/pkg/exitcodes"
	"github.com/golangci/golangci-lint/v2/scripts/bench/internal/changes"
	"github.com/golangci/golangci-lint/v2/scripts/bench/internal/diagnostics"
)

const (
	schemaVersion               = 1
	defaultProfileConcurrency   = 4
	fullCommitSHALength         = 40
	privateFileMode             = 0o600
	privateDirMode              = 0o750
	bytesPerMiB                 = 1024 * 1024
	rssSampleInterval           = 5 * time.Millisecond
	compatibilityCleanupTimeout = 30 * time.Second
	defaultRunTimeout           = 5 * time.Minute
	defaultMaxRSSMiB            = 2048
	defaultGoMaxProcs           = 2
	defaultNice                 = 10
	parentDirectory             = ".."
	forkLabel                   = "fork"
	upstreamLabel               = "upstream"
	binaryOrderForkFirst        = "fork-first"
	binaryOrderUpstreamFirst    = "upstream-first"
	compatibilityModeTimeout    = "timeout"
	compatibilityModeCancel     = "cancel"
	compatibilityModeParallel   = "parallel"
	compatibilityModeCorrupt    = "corrupted-cache"
	compatibilityCLITimeout     = time.Millisecond
	compatibilityCancelAfter    = 25 * time.Millisecond
	compatibilityCancelLimit    = 5 * time.Second
	processExitWait             = time.Second
	processExitPoll             = 10 * time.Millisecond
	benchmarkRunMarkerEnv       = "GOLANGCI_BENCH_RUN_ID"
)

type manifest struct {
	SchemaVersion int        `json:"schema_version"`
	GoVersion     string     `json:"go_version"`
	Concurrency   []int      `json:"concurrency"`
	Runs          int        `json:"runs"`
	Workloads     []workload `json:"workloads"`
	Scenarios     []scenario `json:"scenarios"`
}

type workload struct {
	Name          string   `json:"name"`
	URL           string   `json:"url,omitempty"`
	Path          string   `json:"path,omitempty"`
	Revision      string   `json:"revision"`
	Config        string   `json:"config,omitempty"`
	Modules       []string `json:"modules,omitempty"`
	Packages      []string `json:"packages,omitempty"`
	ProfileModule string   `json:"profile_module,omitempty"`
	Tests         *bool    `json:"tests,omitempty"`
}

type scenario struct {
	Name          string            `json:"name"`
	UseConfig     bool              `json:"use_config,omitempty"`
	WorkDir       string            `json:"work_dir,omitempty"`
	Packages      []string          `json:"packages,omitempty"`
	Args          []string          `json:"args,omitempty"`
	Mutates       bool              `json:"mutates,omitempty"`
	ExpectedFiles map[string]string `json:"expected_files,omitempty"`
	Mode          string            `json:"mode,omitempty"`
}

type options struct {
	ManifestPath       string
	ForkBin            string
	UpstreamBin        string
	OutputDir          string
	Workload           string
	Module             string
	Scenario           string
	Concurrency        string
	CacheMode          string
	BinaryOrder        string
	Runs               int
	Profiles           bool
	Compatibility      bool
	Prepare            bool
	ProfileConcurrency int
	RunTimeout         time.Duration
	MaxRSSMiB          uint64
	GoMaxProcs         int
	Nice               int
}

type binary struct {
	Label string
	Path  string
}

type goToolchain struct {
	Version string
	Root    string
}

type preparedWorkload struct {
	workload
	Root       string
	ConfigPath string
	ConfigHash string
	Targets    []string
	Dirty      bool
}

type metadata struct {
	SchemaVersion int                `json:"schema_version"`
	StartedAt     time.Time          `json:"started_at"`
	Command       []string           `json:"command"`
	Host          hostMetadata       `json:"host"`
	Toolchain     toolchainMetadata  `json:"toolchain"`
	Binaries      []binaryMetadata   `json:"binaries"`
	Workloads     []workloadMetadata `json:"workloads"`
	ManifestHash  string             `json:"manifest_sha256"`
	Limits        limitsMetadata     `json:"limits"`
}

type limitsMetadata struct {
	RunTimeoutNS         int64  `json:"run_timeout_ns"`
	MaxTreeRSSBytes      uint64 `json:"max_tree_rss_bytes"`
	GoMaxProcs           int    `json:"go_max_procs"`
	GoMemoryLimit        string `json:"go_memory_limit"`
	GoBuildParallelism   int    `json:"go_build_parallelism"`
	UnixSchedulingNicety int    `json:"unix_scheduling_nicety"`
}

type hostMetadata struct {
	Hostname    string `json:"hostname"`
	OS          string `json:"os"`
	Arch        string `json:"arch"`
	CPU         string `json:"cpu"`
	LogicalCPUs int    `json:"logical_cpus"`
	MemoryBytes uint64 `json:"memory_bytes"`
}

type toolchainMetadata struct {
	GoVersion string `json:"go_version"`
	GOROOT    string `json:"goroot"`
	GOOS      string `json:"goos"`
	GOARCH    string `json:"goarch"`
}

type binaryMetadata struct {
	Label         string `json:"label"`
	Path          string `json:"path"`
	SHA256        string `json:"sha256"`
	VersionOutput string `json:"version_output"`
}

type workloadMetadata struct {
	Name       string   `json:"name"`
	URL        string   `json:"url,omitempty"`
	Revision   string   `json:"revision"`
	ConfigHash string   `json:"config_sha256,omitempty"`
	Targets    []string `json:"targets"`
	Dirty      bool     `json:"dirty"`
}

type result struct {
	SchemaVersion      int       `json:"schema_version"`
	Binary             string    `json:"binary"`
	Workload           string    `json:"workload"`
	WorkloadRevision   string    `json:"workload_revision"`
	Target             string    `json:"target"`
	Scenario           string    `json:"scenario"`
	CacheMode          string    `json:"cache_mode"`
	Concurrency        int       `json:"concurrency"`
	Iteration          int       `json:"iteration"`
	Purpose            string    `json:"purpose"`
	StartedAt          time.Time `json:"started_at"`
	WallNS             int64     `json:"wall_ns"`
	UserCPUNS          int64     `json:"user_cpu_ns"`
	SystemCPUNS        int64     `json:"system_cpu_ns"`
	PeakTreeRSSBytes   uint64    `json:"peak_tree_rss_bytes"`
	CacheBytesBefore   int64     `json:"cache_bytes_before"`
	CacheBytesAfter    int64     `json:"cache_bytes_after"`
	ExitCode           int       `json:"exit_code"`
	LogPath            string    `json:"log_path"`
	ArtifactPath       string    `json:"artifact_path,omitempty"`
	TerminationReason  string    `json:"termination_reason,omitempty"`
	CancellationNS     int64     `json:"cancellation_latency_ns,omitempty"`
	SurvivingProcesses int       `json:"surviving_processes"`
	Command            []string  `json:"command"`
}

type executionStats struct {
	startedAt          time.Time
	wall               time.Duration
	userCPU            time.Duration
	systemCPU          time.Duration
	peakRSS            uint64
	cacheBefore        int64
	cacheAfter         int64
	exitCode           int
	logPath            string
	artifactPath       string
	terminationReason  string
	cancellation       time.Duration
	survivingProcesses int
	args               []string
}

type memoryStats struct {
	peak     uint64
	exceeded bool
	observed map[int32]struct{}
}

type executionControl struct {
	start       <-chan struct{}
	ready       chan<- error
	marker      string
	cancelAfter time.Duration
}

type processTermination struct {
	pids     []int32
	orphaned int
}

type runner struct {
	ctx       context.Context
	opts      options
	outDir    string
	goRoot    string
	results   *os.File
	binaries  []binary
	workloads []preparedWorkload
	scenarios []scenario
	resultsMu sync.Mutex
}

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "baseline benchmark: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	opts, err := parseOptions(args)
	if err != nil {
		return err
	}

	m, manifestBytes, err := loadManifest(opts.ManifestPath)
	if err != nil {
		return err
	}
	err = applyManifestOverrides(&m, &opts)
	if err != nil {
		return err
	}
	err = validateManifest(&m)
	if err != nil {
		return err
	}

	binaries, err := prepareBinaries(&opts)
	if err != nil {
		return err
	}
	toolchain, err := resolveGoToolchain(ctx, m.GoVersion)
	if err != nil {
		return err
	}
	outDir, err := prepareOutputDir(opts.OutputDir)
	if err != nil {
		return err
	}

	workloads, err := prepareWorkloads(ctx, m.Workloads, outDir, opts.Workload, opts.Module)
	if err != nil {
		return err
	}
	scenarios, err := filterScenarios(m.Scenarios, opts.Scenario)
	if err != nil {
		return err
	}

	metadataValue, err := collectMetadata(ctx, manifestBytes, toolchain, binaries, workloads, &opts)
	if err != nil {
		return err
	}
	writeErr := writeJSON(filepath.Join(outDir, "metadata.json"), metadataValue)
	if writeErr != nil {
		return writeErr
	}

	resultsFile, err := os.OpenFile(
		filepath.Join(outDir, "results.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, privateFileMode)
	if err != nil {
		return fmt.Errorf("create results file: %w", err)
	}
	defer resultsFile.Close()

	r := runner{
		ctx:       ctx,
		opts:      opts,
		outDir:    outDir,
		goRoot:    toolchain.Root,
		results:   resultsFile,
		binaries:  binaries,
		workloads: workloads,
		scenarios: scenarios,
	}
	_, _ = fmt.Fprintf(os.Stdout, "benchmark limits: timeout=%s, RSS=%d MiB, GOMAXPROCS=%d, nice=%d\n",
		opts.RunTimeout, opts.MaxRSSMiB, opts.GoMaxProcs, opts.Nice)
	runErr := r.runAll(m.Concurrency, m.Runs)
	if runErr != nil {
		return runErr
	}

	_, _ = fmt.Fprintf(os.Stdout, "baseline artifacts: %s\n", outDir)

	return nil
}

func applyManifestOverrides(m *manifest, opts *options) error {
	if opts.Runs > 0 {
		m.Runs = opts.Runs
	}
	if opts.Concurrency == "" {
		return nil
	}

	values, err := parsePositiveInts(opts.Concurrency)
	if err != nil {
		return fmt.Errorf("parse concurrency: %w", err)
	}
	m.Concurrency = values

	return nil
}

func resolveGoToolchain(ctx context.Context, requiredVersion string) (goToolchain, error) {
	version, err := commandOutput(ctx, "go", "version")
	if err != nil {
		return goToolchain{}, fmt.Errorf("read Go version: %w", err)
	}
	if !strings.Contains(version, " "+requiredVersion+" ") {
		return goToolchain{}, fmt.Errorf("manifest requires %s, got %s", requiredVersion, version)
	}

	root, err := commandOutput(ctx, "go", "env", "GOROOT")
	if err != nil {
		return goToolchain{}, fmt.Errorf("read GOROOT: %w", err)
	}
	_, statErr := os.Stat(filepath.Join(root, "bin", "go"))
	if statErr != nil {
		return goToolchain{}, fmt.Errorf("validate GOROOT: %w", statErr)
	}

	return goToolchain{Version: version, Root: root}, nil
}

func parseOptions(args []string) (options, error) {
	var opts options

	fs := flag.NewFlagSet("baseline", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&opts.ManifestPath, "manifest", "scripts/bench/baseline.json", "benchmark manifest")
	fs.StringVar(&opts.ForkBin, "fork-bin", "", "fork binary")
	fs.StringVar(&opts.UpstreamBin, "upstream-bin", "", "upstream binary")
	fs.StringVar(&opts.OutputDir, "out", "", "artifact directory")
	fs.StringVar(&opts.Workload, "workload", "", "workload name filter")
	fs.StringVar(&opts.Module, "module", "", "module path filter")
	fs.StringVar(&opts.Scenario, "scenario", "", "scenario name filter")
	fs.StringVar(&opts.Concurrency, "concurrency", "", "comma-separated concurrency values")
	fs.StringVar(&opts.CacheMode, "cache-mode", "cold,warm", "cold, warm, or both")
	fs.StringVar(&opts.BinaryOrder, "binary-order", binaryOrderForkFirst,
		"timing order: fork-first or upstream-first")
	fs.IntVar(&opts.Runs, "runs", 0, "runs per case; manifest value by default")
	fs.BoolVar(&opts.Profiles, "profiles", false, "capture separate CPU, heap, and trace profiles")
	fs.BoolVar(&opts.Compatibility, "compatibility", false, "compare diagnostics from fork and upstream binaries")
	fs.BoolVar(&opts.Prepare, "prepare", true, "prewarm the Go build and module caches")
	fs.IntVar(&opts.ProfileConcurrency, "profile-concurrency", defaultProfileConcurrency, "concurrency for profile runs")
	fs.DurationVar(&opts.RunTimeout, "run-timeout", defaultRunTimeout, "hard timeout for each golangci-lint process")
	fs.Uint64Var(&opts.MaxRSSMiB, "max-rss-mib", defaultMaxRSSMiB, "kill a process tree above this RSS")
	fs.IntVar(&opts.GoMaxProcs, "go-max-procs", defaultGoMaxProcs, "GOMAXPROCS and Go build parallelism limit")
	fs.IntVar(&opts.Nice, "nice", defaultNice, "Unix scheduling nicety from 0 to 20")
	if err := fs.Parse(args); err != nil {
		return options{}, fmt.Errorf("parse flags: %w", err)
	}
	if fs.NArg() != 0 {
		return options{}, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if err := validateOptions(&opts); err != nil {
		return options{}, err
	}

	return opts, nil
}

func validateOptions(opts *options) error {
	if opts.ForkBin == "" && opts.UpstreamBin == "" {
		return errors.New("at least one of --fork-bin or --upstream-bin is required")
	}
	if opts.Runs < 0 {
		return errors.New("--runs cannot be negative")
	}
	if opts.ProfileConcurrency < 1 {
		return errors.New("--profile-concurrency must be positive")
	}
	if opts.RunTimeout <= 0 {
		return errors.New("--run-timeout must be positive")
	}
	if opts.MaxRSSMiB == 0 {
		return errors.New("--max-rss-mib must be positive")
	}
	if opts.MaxRSSMiB > math.MaxUint64/bytesPerMiB {
		return errors.New("--max-rss-mib is too large")
	}
	if opts.GoMaxProcs < 1 {
		return errors.New("--go-max-procs must be positive")
	}
	if opts.Nice < 0 || opts.Nice > 20 {
		return errors.New("--nice must be between 0 and 20")
	}
	if _, err := parseCacheModes(opts.CacheMode); err != nil {
		return err
	}
	if opts.BinaryOrder != binaryOrderForkFirst && opts.BinaryOrder != binaryOrderUpstreamFirst {
		return fmt.Errorf("invalid binary order %q", opts.BinaryOrder)
	}

	return validateCompatibilityOptions(opts)
}

func validateCompatibilityOptions(opts *options) error {
	if !opts.Compatibility {
		return nil
	}
	if opts.ForkBin == "" || opts.UpstreamBin == "" {
		return errors.New("--compatibility requires --fork-bin and --upstream-bin")
	}
	if opts.Workload == "" {
		return errors.New("--compatibility requires an explicit --workload")
	}
	if opts.Concurrency == "" {
		return errors.New("--compatibility requires explicit --concurrency values")
	}
	if opts.Profiles {
		return errors.New("--compatibility cannot capture profiles")
	}

	return nil
}

func loadManifest(path string) (manifest, []byte, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return manifest{}, nil, fmt.Errorf("read manifest: %w", err)
	}

	var m manifest
	dec := json.NewDecoder(bytes.NewReader(content))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return manifest{}, nil, fmt.Errorf("decode manifest: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return manifest{}, nil, errors.New("decode manifest: trailing data")
	}

	return m, content, nil
}

func validateManifest(m *manifest) error {
	if m.SchemaVersion != schemaVersion {
		return fmt.Errorf("unsupported manifest schema %d", m.SchemaVersion)
	}
	if m.GoVersion == "" {
		return errors.New("manifest go_version is required")
	}
	if m.Runs < 1 {
		return errors.New("manifest runs must be positive")
	}
	if len(m.Concurrency) == 0 {
		return errors.New("manifest concurrency is required")
	}
	for _, value := range m.Concurrency {
		if value < 1 {
			return errors.New("manifest concurrency values must be positive")
		}
	}
	if len(m.Workloads) == 0 {
		return errors.New("manifest workloads are required")
	}
	if len(m.Scenarios) == 0 {
		return errors.New("manifest scenarios are required")
	}

	if err := validateWorkloads(m.Workloads); err != nil {
		return err
	}

	return validateScenarios(m.Scenarios)
}

func validateWorkloads(workloads []workload) error {
	names := make(map[string]struct{}, len(workloads))
	for i := range workloads {
		item := &workloads[i]
		if err := validateName("workload", item.Name, names); err != nil {
			return err
		}
		if (item.URL == "") == (item.Path == "") {
			return fmt.Errorf("workload %q requires exactly one of url or path", item.Name)
		}
		if len(item.Revision) != fullCommitSHALength {
			return fmt.Errorf("workload %q revision must be a full commit SHA", item.Name)
		}
		if _, err := hex.DecodeString(item.Revision); err != nil {
			return fmt.Errorf("workload %q revision: %w", item.Name, err)
		}
	}

	return nil
}

func validateScenarios(scenarios []scenario) error {
	names := make(map[string]struct{}, len(scenarios))
	for i := range scenarios {
		item := &scenarios[i]
		if err := validateName("scenario", item.Name, names); err != nil {
			return err
		}
		if err := validateScenario(item); err != nil {
			return err
		}
	}

	return nil
}

func validateScenario(item *scenario) error {
	if err := validateScenarioExecution(item); err != nil {
		return err
	}
	if item.UseConfig && slices.Contains(item.Args, "--no-config") {
		return fmt.Errorf("scenario %q cannot use config and --no-config", item.Name)
	}
	if filepath.IsAbs(item.WorkDir) {
		return fmt.Errorf("scenario %q working directory must be relative", item.Name)
	}
	if item.WorkDir == parentDirectory ||
		strings.HasPrefix(filepath.Clean(item.WorkDir), parentDirectory+string(filepath.Separator)) {
		return fmt.Errorf("scenario %q working directory escapes the workload target", item.Name)
	}
	for _, pkg := range item.Packages {
		if strings.TrimSpace(pkg) == "" {
			return fmt.Errorf("scenario %q packages cannot contain an empty value", item.Name)
		}
	}
	if len(item.ExpectedFiles) > 0 && !item.Mutates {
		return fmt.Errorf("scenario %q expected_files requires mutates", item.Name)
	}

	return validateExpectedFiles(item)
}

func validateScenarioExecution(item *scenario) error {
	if !slices.Contains([]string{
		"", compatibilityModeTimeout, compatibilityModeCancel,
		compatibilityModeParallel, compatibilityModeCorrupt,
	}, item.Mode) {
		return fmt.Errorf("scenario %q has unsupported mode %q", item.Name, item.Mode)
	}
	if item.Mode != "" && item.Mutates {
		return fmt.Errorf("scenario %q cannot combine mode %q with mutates", item.Name, item.Mode)
	}
	if slices.ContainsFunc(item.Args, func(value string) bool {
		return value == "--fix" || strings.HasPrefix(value, "--fix=")
	}) {
		return fmt.Errorf("scenario %q must use mutates instead of a fix argument", item.Name)
	}
	if slices.ContainsFunc(item.Args, func(value string) bool {
		return value == "--timeout" || strings.HasPrefix(value, "--timeout=") ||
			value == "--allow-parallel-runners" || value == "--allow-serial-runners"
	}) {
		return fmt.Errorf("scenario %q execution controls are managed by mode", item.Name)
	}

	return nil
}

func validateExpectedFiles(item *scenario) error {
	for actual, expected := range item.ExpectedFiles {
		if err := validateRelativePath(actual); err != nil {
			return fmt.Errorf("scenario %q expected file %q: %w", item.Name, actual, err)
		}
		if err := validateRelativePath(expected); err != nil {
			return fmt.Errorf("scenario %q golden file %q: %w", item.Name, expected, err)
		}
	}

	return nil
}

func validateRelativePath(path string) error {
	if path == "" {
		return errors.New("path cannot be empty")
	}
	if filepath.IsAbs(path) {
		return errors.New("path must be relative")
	}
	clean := filepath.Clean(path)
	if clean == parentDirectory || strings.HasPrefix(clean, parentDirectory+string(filepath.Separator)) {
		return errors.New("path escapes the workload root")
	}

	return nil
}

func validateName(kind, name string, seen map[string]struct{}) error {
	if name == "" || safeName(name) != name {
		return fmt.Errorf("%s name %q must contain only letters, digits, dot, dash, or underscore", kind, name)
	}
	if _, ok := seen[name]; ok {
		return fmt.Errorf("duplicate %s name %q", kind, name)
	}
	seen[name] = struct{}{}

	return nil
}

func parsePositiveInts(raw string) ([]int, error) {
	var values []int
	seen := make(map[int]struct{})
	for _, part := range strings.Split(raw, ",") {
		value, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || value < 1 {
			return nil, fmt.Errorf("invalid positive integer %q", part)
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		values = append(values, value)
	}
	if len(values) == 0 {
		return nil, errors.New("at least one value is required")
	}

	return values, nil
}

func parseCacheModes(raw string) ([]string, error) {
	var modes []string
	for _, value := range strings.Split(raw, ",") {
		mode := strings.TrimSpace(value)
		if mode != "cold" && mode != "warm" {
			return nil, fmt.Errorf("invalid cache mode %q", mode)
		}
		if !slices.Contains(modes, mode) {
			modes = append(modes, mode)
		}
	}
	if len(modes) == 0 {
		return nil, errors.New("at least one cache mode is required")
	}

	return modes, nil
}

func prepareBinaries(opts *options) ([]binary, error) {
	var binaries []binary
	for _, item := range configuredBinaries(opts) {
		if item.Path == "" {
			continue
		}
		path, err := filepath.Abs(item.Path)
		if err != nil {
			return nil, fmt.Errorf("resolve %s binary: %w", item.Label, err)
		}
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("stat %s binary: %w", item.Label, err)
		}
		if info.IsDir() || info.Mode()&0o111 == 0 {
			return nil, fmt.Errorf("%s binary is not executable: %s", item.Label, path)
		}
		item.Path = path
		binaries = append(binaries, item)
	}

	return binaries, nil
}

func configuredBinaries(opts *options) []binary {
	fork := binary{Label: forkLabel, Path: opts.ForkBin}
	upstream := binary{Label: upstreamLabel, Path: opts.UpstreamBin}
	if opts.BinaryOrder == binaryOrderUpstreamFirst {
		return []binary{upstream, fork}
	}
	return []binary{fork, upstream}
}

func prepareOutputDir(path string) (string, error) {
	if path == "" {
		path = filepath.Join("dist", "bench", time.Now().UTC().Format("20060102T150405Z"))
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve output directory: %w", err)
	}
	if _, err := os.Stat(abs); err == nil {
		return "", fmt.Errorf("output directory already exists: %s", abs)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("stat output directory: %w", err)
	}
	for _, dir := range []string{"logs", "profiles", "cache", "workloads", "compat/raw"} {
		if err := os.MkdirAll(filepath.Join(abs, dir), privateDirMode); err != nil {
			return "", fmt.Errorf("create output directory: %w", err)
		}
	}

	return abs, nil
}

func prepareWorkloads(ctx context.Context, configured []workload, outDir, workloadFilter, moduleFilter string) ([]preparedWorkload, error) {
	var workloads []preparedWorkload
	matchedWorkload := false
	for i := range configured {
		item := &configured[i]
		if workloadFilter != "" && item.Name != workloadFilter {
			continue
		}
		matchedWorkload = true

		prepared, err := prepareWorkload(ctx, item, filepath.Join(outDir, "workloads", item.Name))
		if err != nil {
			return nil, fmt.Errorf("prepare workload %q: %w", item.Name, err)
		}
		if moduleFilter != "" {
			prepared.Targets = filterTargets(prepared.Targets, moduleFilter)
			if len(prepared.Targets) == 0 {
				continue
			}
			if !slices.Contains(prepared.Targets, prepared.ProfileModule) {
				prepared.ProfileModule = prepared.Targets[0]
			}
		}
		workloads = append(workloads, prepared)
	}
	if len(workloads) == 0 {
		if !matchedWorkload {
			return nil, fmt.Errorf("unknown workload %q", workloadFilter)
		}
		if moduleFilter != "" {
			return nil, fmt.Errorf("unknown module %q", moduleFilter)
		}
		return nil, fmt.Errorf("unknown workload %q", workloadFilter)
	}

	return workloads, nil
}

func filterTargets(configured []string, filter string) []string {
	for _, target := range configured {
		if target == filter {
			return []string{target}
		}
	}

	return nil
}

func prepareWorkload(ctx context.Context, item *workload, destination string) (preparedWorkload, error) {
	root, err := materializeWorkload(ctx, item, destination)
	if err != nil {
		return preparedWorkload{}, err
	}
	revision, err := commandOutput(ctx, "git", "-C", root, "rev-parse", "HEAD")
	if err != nil {
		return preparedWorkload{}, fmt.Errorf("read revision: %w", err)
	}
	if revision != item.Revision {
		return preparedWorkload{}, fmt.Errorf("workload is at %s, expected %s", revision, item.Revision)
	}

	targets, err := resolveTargets(root, item.Modules)
	if err != nil {
		return preparedWorkload{}, err
	}
	configPath, configHash, err := resolveConfig(root, item.Config)
	if err != nil {
		return preparedWorkload{}, err
	}
	dirty, err := isWorkloadDirty(ctx, root)
	if err != nil {
		return preparedWorkload{}, err
	}

	return preparedWorkload{
		workload:   *item,
		Root:       root,
		ConfigPath: configPath,
		ConfigHash: configHash,
		Targets:    targets,
		Dirty:      dirty,
	}, nil
}

func resolveTargets(root string, configured []string) ([]string, error) {
	targets := configured
	if len(targets) == 0 {
		targets = []string{"."}
	}
	for _, target := range targets {
		_, err := safeJoin(root, target)
		if err != nil {
			return nil, fmt.Errorf("module: %w", err)
		}
	}

	return targets, nil
}

func resolveConfig(root, configured string) (path, hash string, err error) {
	if configured == "" {
		return "", "", nil
	}
	path, err = safeJoin(root, configured)
	if err != nil {
		return "", "", fmt.Errorf("config: %w", err)
	}
	hash, err = fileSHA256(path)
	if err != nil {
		return "", "", fmt.Errorf("hash config: %w", err)
	}

	return path, hash, nil
}

func isWorkloadDirty(ctx context.Context, root string) (bool, error) {
	output, err := commandOutput(ctx, "git", "-C", root, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return false, fmt.Errorf("check state: %w", err)
	}

	return output != "", nil
}

func materializeWorkload(ctx context.Context, item *workload, destination string) (string, error) {
	if item.Path != "" {
		path, err := filepath.Abs(item.Path)
		if err != nil {
			return "", fmt.Errorf("resolve local path: %w", err)
		}
		if _, err := os.Stat(path); err != nil {
			return "", fmt.Errorf("stat local path: %w", err)
		}

		return path, nil
	}

	if err := runCommand(ctx, "", io.Discard, "git", "clone", "--filter=blob:none", "--no-checkout", item.URL, destination); err != nil {
		return "", err
	}
	if err := runCommand(ctx, "", io.Discard, "git", "-C", destination, "fetch", "--depth=1", "origin", item.Revision); err != nil {
		return "", err
	}
	if err := runCommand(ctx, "", io.Discard, "git", "-C", destination, "checkout", "--detach", item.Revision); err != nil {
		return "", err
	}

	return destination, nil
}

func safeJoin(root, relative string) (string, error) {
	if filepath.IsAbs(relative) {
		return "", fmt.Errorf("path must be relative: %s", relative)
	}
	path := filepath.Join(root, filepath.Clean(relative))
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	if rel == parentDirectory || strings.HasPrefix(rel, parentDirectory+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes workload root: %s", relative)
	}
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("stat path %s: %w", relative, err)
	}

	return path, nil
}

func filterScenarios(configured []scenario, filter string) ([]scenario, error) {
	var scenarios []scenario
	for _, item := range configured {
		if filter == "" || item.Name == filter {
			scenarios = append(scenarios, item)
		}
	}
	if len(scenarios) == 0 {
		return nil, fmt.Errorf("unknown scenario %q", filter)
	}

	return scenarios, nil
}

func collectMetadata(
	ctx context.Context,
	manifestBytes []byte,
	toolchain goToolchain,
	binaries []binary,
	workloads []preparedWorkload,
	opts *options,
) (metadata, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return metadata{}, fmt.Errorf("read hostname: %w", err)
	}
	cpuInfo, err := cpu.InfoWithContext(ctx)
	if err != nil {
		return metadata{}, fmt.Errorf("read CPU info: %w", err)
	}
	memory, err := mem.VirtualMemoryWithContext(ctx)
	if err != nil {
		return metadata{}, fmt.Errorf("read memory info: %w", err)
	}
	cpuModel := "unknown"
	if len(cpuInfo) > 0 {
		cpuModel = cpuInfo[0].ModelName
	}

	value := metadata{
		SchemaVersion: schemaVersion,
		StartedAt:     time.Now().UTC(),
		Command:       append([]string{"go", "run", "./scripts/bench/baseline"}, os.Args[1:]...),
		Host: hostMetadata{
			Hostname:    hostname,
			OS:          runtime.GOOS,
			Arch:        runtime.GOARCH,
			CPU:         cpuModel,
			LogicalCPUs: runtime.NumCPU(),
			MemoryBytes: memory.Total,
		},
		Toolchain: toolchainMetadata{
			GoVersion: toolchain.Version,
			GOROOT:    toolchain.Root,
			GOOS:      runtime.GOOS,
			GOARCH:    runtime.GOARCH,
		},
		ManifestHash: bytesSHA256(manifestBytes),
		Limits: limitsMetadata{
			RunTimeoutNS:         opts.RunTimeout.Nanoseconds(),
			MaxTreeRSSBytes:      opts.MaxRSSMiB * bytesPerMiB,
			GoMaxProcs:           opts.GoMaxProcs,
			GoMemoryLimit:        fmt.Sprintf("%dMiB", opts.MaxRSSMiB),
			GoBuildParallelism:   opts.GoMaxProcs,
			UnixSchedulingNicety: opts.Nice,
		},
	}
	for _, item := range binaries {
		hash, err := fileSHA256(item.Path)
		if err != nil {
			return metadata{}, fmt.Errorf("hash %s binary: %w", item.Label, err)
		}
		versionOutput, err := commandOutput(ctx, item.Path, "--color=never", "version", "--json")
		if err != nil {
			return metadata{}, fmt.Errorf("read %s binary version: %w", item.Label, err)
		}
		value.Binaries = append(value.Binaries, binaryMetadata{
			Label:         item.Label,
			Path:          item.Path,
			SHA256:        hash,
			VersionOutput: versionOutput,
		})
	}
	for i := range workloads {
		item := &workloads[i]
		value.Workloads = append(value.Workloads, workloadMetadata{
			Name:       item.Name,
			URL:        item.URL,
			Revision:   item.Revision,
			ConfigHash: item.ConfigHash,
			Targets:    item.Targets,
			Dirty:      item.Dirty,
		})
	}

	return value, nil
}

func (r *runner) runAll(concurrency []int, runs int) error {
	if r.opts.Compatibility {
		return r.runCompatibility(concurrency)
	}
	if err := r.runTimingMatrix(concurrency, runs); err != nil {
		return err
	}
	if !r.opts.Profiles {
		return nil
	}

	return r.runProfiles()
}

func (r *runner) runTimingMatrix(concurrency []int, runs int) error {
	modes, _ := parseCacheModes(r.opts.CacheMode)
	for i := range r.workloads {
		workload := &r.workloads[i]
		for _, target := range workload.Targets {
			for scenarioIndex := range r.scenarios {
				scenario := &r.scenarios[scenarioIndex]
				for _, value := range concurrency {
					for _, bin := range r.binaries {
						if r.opts.Prepare {
							cacheDir := r.cacheDir(bin, workload, target, scenario, value, "prepare")
							if _, err := r.execute(bin, workload, target, scenario, value, 0, "prepare", "prepare", cacheDir, ""); err != nil {
								return err
							}
						}
						for _, mode := range modes {
							if mode == "warm" {
								cacheDir := r.cacheDir(bin, workload, target, scenario, value, "warm")
								if _, err := r.execute(bin, workload, target, scenario, value, 0, mode, "warm-seed", cacheDir, ""); err != nil {
									return err
								}
							}
							for iteration := 1; iteration <= runs; iteration++ {
								cacheKey := mode
								if mode == "cold" {
									cacheKey = fmt.Sprintf("cold-%d", iteration)
								}
								cacheDir := r.cacheDir(bin, workload, target, scenario, value, cacheKey)
								if _, err := r.execute(bin, workload, target, scenario, value, iteration, mode, "timing", cacheDir, ""); err != nil {
									return err
								}
							}
						}
					}
				}
			}
		}
	}

	return nil
}

func (r *runner) runCompatibility(concurrency []int) error {
	binaries := make(map[string]binary, len(r.binaries))
	for _, bin := range r.binaries {
		binaries[bin.Label] = bin
	}

	for i := range r.workloads {
		workload := &r.workloads[i]
		for _, target := range workload.Targets {
			for scenarioIndex := range r.scenarios {
				scenario := &r.scenarios[scenarioIndex]
				for _, value := range concurrency {
					if scenario.Mode != "" {
						if err := r.runSpecialCompatibility(binaries, workload, target, scenario, value); err != nil {
							return err
						}
						continue
					}
					if scenario.Mutates {
						if err := r.runMutationCompatibility(binaries, workload, target, scenario, value); err != nil {
							return err
						}
						continue
					}
					inputs := make(map[string]diagnostics.Input, len(binaries))
					for _, label := range []string{upstreamLabel, forkLabel} {
						bin := binaries[label]
						base := artifactBase(bin, workload, target, scenario, value, 1, "cold", "compatibility")
						artifact := filepath.Join(r.outDir, "compat", "raw", base+".json")
						cacheDir := r.cacheDir(bin, workload, target, scenario, value, "compatibility")
						stats, err := r.execute(
							bin, workload, target, scenario, value, 1, "cold", "compatibility", cacheDir, artifact,
							compatibilityOutputArgs(artifact)...,
						)
						if err != nil {
							return err
						}
						inputs[label] = compatibilityInput(artifact, workload.Root, stats, "", false)
					}

					caseName := compatibilityCaseBase(workload, target, scenario, value)
					outputDir := filepath.Join(r.outDir, "compat", caseName)
					summary, err := diagnostics.CompareFiles(
						inputs[upstreamLabel], inputs[forkLabel], outputDir,
					)
					if err != nil {
						return fmt.Errorf("compare %s: %w", caseName, err)
					}
					_, _ = fmt.Fprintf(
						os.Stdout, "compatibility %s: %d issues, exit code %d\n",
						caseName, summary.ReferenceIssues, summary.ReferenceExit,
					)
				}
			}
		}
	}

	return nil
}

func (r *runner) runSpecialCompatibility(
	binaries map[string]binary,
	workload *preparedWorkload,
	target string,
	scenario *scenario,
	concurrency int,
) error {
	switch scenario.Mode {
	case compatibilityModeTimeout, compatibilityModeCancel:
		return r.runInterruptedCompatibility(binaries, workload, target, scenario, concurrency)
	case compatibilityModeParallel:
		return r.runParallelCompatibility(binaries, workload, target, scenario, concurrency)
	case compatibilityModeCorrupt:
		return r.runCorruptedCacheCompatibility(binaries, workload, target, scenario, concurrency)
	default:
		return fmt.Errorf("unsupported compatibility mode %q", scenario.Mode)
	}
}

func (r *runner) runInterruptedCompatibility(
	binaries map[string]binary,
	workload *preparedWorkload,
	target string,
	scenario *scenario,
	concurrency int,
) error {
	inputs := make(map[string]diagnostics.Input, len(binaries))
	for _, label := range []string{upstreamLabel, forkLabel} {
		bin := binaries[label]
		purpose := "compatibility-" + scenario.Mode
		artifact := r.compatibilityArtifact(bin, workload, target, scenario, concurrency, purpose)
		cacheDir := r.cacheDir(bin, workload, target, scenario, concurrency, purpose)
		control := executionControl{}
		if scenario.Mode == compatibilityModeCancel {
			control.cancelAfter = compatibilityCancelAfter
		}
		stats, err := r.executeControlled(
			bin, workload, target, scenario, concurrency, 1, "cold", purpose, cacheDir, artifact, control,
			compatibilityOutputArgs(artifact)...,
		)
		if err != nil {
			return err
		}
		termination := "cli_timeout"
		if scenario.Mode == compatibilityModeTimeout {
			if stats.exitCode != exitcodes.Timeout {
				return fmt.Errorf("%s timeout exited with %d, expected %d", label, stats.exitCode, exitcodes.Timeout)
			}
		} else {
			termination = stats.terminationReason
			if termination != compatibilityModeCancel {
				return fmt.Errorf("%s cancellation ended as %q", label, termination)
			}
			if stats.cancellation > compatibilityCancelLimit {
				return fmt.Errorf("%s cancellation took %s", label, stats.cancellation)
			}
		}
		inputs[label] = compatibilityInput(artifact, workload.Root, stats, termination, true)
	}

	return r.compareCompatibilityCase(compatibilityCaseBase(workload, target, scenario, concurrency), inputs)
}

func (r *runner) runParallelCompatibility(
	binaries map[string]binary,
	workload *preparedWorkload,
	target string,
	scenario *scenario,
	concurrency int,
) error {
	inputs := make(map[string][]diagnostics.Input, len(binaries))
	for _, label := range []string{upstreamLabel, forkLabel} {
		values, err := r.runParallelBinary(binaries[label], workload, target, scenario, concurrency)
		if err != nil {
			return err
		}
		inputs[label] = values
	}

	caseName := compatibilityCaseBase(workload, target, scenario, concurrency)
	for index := range inputs[upstreamLabel] {
		pair := map[string]diagnostics.Input{
			upstreamLabel: inputs[upstreamLabel][index],
			forkLabel:     inputs[forkLabel][index],
		}
		if err := r.compareCompatibilityCase(
			filepath.Join(caseName, fmt.Sprintf("run-%d", index+1)), pair,
		); err != nil {
			return err
		}
	}

	return nil
}

func (r *runner) runParallelBinary(
	bin binary,
	workload *preparedWorkload,
	target string,
	scenario *scenario,
	concurrency int,
) ([]diagnostics.Input, error) {
	const processCount = 2
	type executionResult struct {
		index    int
		artifact string
		stats    *executionStats
		err      error
	}

	cacheDir := r.cacheDir(bin, workload, target, scenario, concurrency, "compatibility-parallel")
	start := make(chan struct{})
	ready := make(chan error, processCount)
	results := make(chan executionResult, processCount)
	for index := range processCount {
		go func() {
			purpose := fmt.Sprintf("compatibility-parallel-%d", index+1)
			artifact := r.compatibilityArtifact(bin, workload, target, scenario, concurrency, purpose)
			stats, err := r.executeControlled(
				bin, workload, target, scenario, concurrency, 1, "cold", purpose, cacheDir, artifact,
				executionControl{start: start, ready: ready}, compatibilityOutputArgs(artifact)...,
			)
			results <- executionResult{index: index, artifact: artifact, stats: stats, err: err}
		}()
	}
	var readinessErr error
	for range processCount {
		readinessErr = errors.Join(readinessErr, <-ready)
	}
	close(start)

	inputs := make([]diagnostics.Input, processCount)
	resultErr := readinessErr
	for range processCount {
		result := <-results
		if result.err != nil {
			resultErr = errors.Join(resultErr, result.err)
			continue
		}
		inputs[result.index] = compatibilityInput(result.artifact, workload.Root, result.stats, "", false)
	}
	if resultErr != nil {
		return nil, resultErr
	}

	return inputs, nil
}

func (r *runner) runCorruptedCacheCompatibility(
	binaries map[string]binary,
	workload *preparedWorkload,
	target string,
	scenario *scenario,
	concurrency int,
) error {
	inputs := make(map[string]diagnostics.Input, len(binaries))
	for _, label := range []string{upstreamLabel, forkLabel} {
		bin := binaries[label]
		cacheDir := r.cacheDir(bin, workload, target, scenario, concurrency, "compatibility-corrupted-cache")
		seedPurpose := "compatibility-corrupted-cache-seed"
		seedArtifact := r.compatibilityArtifact(bin, workload, target, scenario, concurrency, seedPurpose)
		if _, err := r.execute(
			bin, workload, target, scenario, concurrency, 1, "cold", seedPurpose, cacheDir, seedArtifact,
			compatibilityOutputArgs(seedArtifact)...,
		); err != nil {
			return err
		}
		corrupted, err := corruptCacheData(cacheDir)
		if err != nil {
			return fmt.Errorf("corrupt %s cache: %w", label, err)
		}
		if corrupted == 0 {
			return fmt.Errorf("corrupt %s cache: no data entries found", label)
		}

		purpose := "compatibility-corrupted-cache"
		artifact := r.compatibilityArtifact(bin, workload, target, scenario, concurrency, purpose)
		stats, err := r.execute(
			bin, workload, target, scenario, concurrency, 1, "warm", purpose, cacheDir, artifact,
			compatibilityOutputArgs(artifact)...,
		)
		if err != nil {
			return err
		}
		inputs[label] = compatibilityInput(artifact, workload.Root, stats, "", false)
	}

	return r.compareCompatibilityCase(compatibilityCaseBase(workload, target, scenario, concurrency), inputs)
}

func compatibilityInput(
	artifact string,
	root string,
	stats *executionStats,
	termination string,
	optionalReport bool,
) diagnostics.Input {
	return diagnostics.Input{
		Path: artifact,
		Root: root,
		Outcome: &diagnostics.ProcessOutcome{
			ExitCode:              stats.exitCode,
			TerminationReason:     termination,
			OptionalReport:        optionalReport,
			CancellationLatencyNS: stats.cancellation.Nanoseconds(),
			SurvivingProcesses:    stats.survivingProcesses,
		},
	}
}

func (r *runner) compareCompatibilityCase(caseName string, inputs map[string]diagnostics.Input) error {
	outputDir := filepath.Join(r.outDir, "compat", caseName)
	summary, err := diagnostics.CompareFiles(inputs[upstreamLabel], inputs[forkLabel], outputDir)
	if err != nil {
		return fmt.Errorf("compare %s: %w", caseName, err)
	}
	_, _ = fmt.Fprintf(
		os.Stdout, "compatibility %s: %d issues, exit code %d, termination %q\n",
		caseName, summary.ReferenceIssues, summary.ReferenceExit, summary.ReferenceTermination,
	)

	return nil
}

func (r *runner) compatibilityArtifact(
	bin binary,
	workload *preparedWorkload,
	target string,
	scenario *scenario,
	concurrency int,
	purpose string,
) string {
	base := artifactBase(bin, workload, target, scenario, concurrency, 1, "cold", purpose)
	return filepath.Join(r.outDir, "compat", "raw", base+".json")
}

func corruptCacheData(root string) (int, error) {
	var paths []string
	if err := filepath.WalkDir(root, func(path string, entry iofs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), "-d") {
			paths = append(paths, path)
		}

		return nil
	}); err != nil {
		return 0, err
	}
	slices.Sort(paths)
	for _, path := range paths {
		if err := os.WriteFile(path, []byte("corrupted cache data\n"), privateFileMode); err != nil {
			return 0, err
		}
	}

	return len(paths), nil
}

func (r *runner) runMutationCompatibility(
	binaries map[string]binary,
	workload *preparedWorkload,
	target string,
	scenario *scenario,
	concurrency int,
) (runErr error) {
	caseName := compatibilityCaseBase(workload, target, scenario, concurrency)
	worktrees, created, baseline, err := r.prepareMutationWorktrees(workload, caseName)
	if err != nil {
		return err
	}
	retain := false
	defer func() {
		if !retain {
			runErr = errors.Join(runErr, r.removeCompatibilityWorktrees(workload.Root, created))
		}
	}()

	expectedContent, err := loadExpectedFiles(worktrees[upstreamLabel].Root, scenario.ExpectedFiles)
	if err != nil {
		return err
	}

	inputs := make(map[string]diagnostics.Input, len(binaries))
	for _, label := range []string{upstreamLabel, forkLabel} {
		input, inputErr := r.runMutationBinary(
			binaries[label], worktrees[label], target, scenario, concurrency, baseline, expectedContent,
		)
		if inputErr != nil {
			return inputErr
		}
		inputs[label] = input
	}

	outputDir := filepath.Join(r.outDir, "compat", caseName)
	summary, err := diagnostics.CompareFiles(inputs[upstreamLabel], inputs[forkLabel], outputDir)
	if err != nil {
		if summary.SchemaVersion != 0 && !summary.Match {
			retain = true
		}
		return fmt.Errorf("compare %s: %w", caseName, err)
	}
	_, _ = fmt.Fprintf(
		os.Stdout, "compatibility %s: %d issues, exit code %d, %d file changes\n",
		caseName, summary.ReferenceIssues, summary.ReferenceExit, len(inputs[upstreamLabel].Changes.Changes),
	)

	return nil
}

func (r *runner) prepareMutationWorktrees(
	workload *preparedWorkload,
	caseName string,
) (_ map[string]*preparedWorkload, _ []*preparedWorkload, _ changes.Snapshot, runErr error) {
	worktrees := make(map[string]*preparedWorkload, 2)
	var created []*preparedWorkload
	defer func() {
		if runErr != nil {
			runErr = errors.Join(runErr, r.removeCompatibilityWorktrees(workload.Root, created))
		}
	}()

	var baseline changes.Snapshot
	for _, label := range []string{upstreamLabel, forkLabel} {
		isolated, err := r.createCompatibilityWorktree(workload, caseName, label)
		if err != nil {
			return nil, nil, changes.Snapshot{}, err
		}
		created = append(created, isolated)
		worktrees[label] = isolated

		snapshot, err := changes.Capture(isolated.Root)
		if err != nil {
			return nil, nil, changes.Snapshot{}, err
		}
		if label == upstreamLabel {
			baseline = snapshot
		} else if !changes.Equal(baseline, snapshot) {
			return nil, nil, changes.Snapshot{},
				fmt.Errorf("compatibility worktrees for %s do not start identically", caseName)
		}
	}

	return worktrees, created, baseline, nil
}

func (r *runner) runMutationBinary(
	bin binary,
	workload *preparedWorkload,
	target string,
	scenario *scenario,
	concurrency int,
	baseline changes.Snapshot,
	expectedContent map[string][]byte,
) (diagnostics.Input, error) {
	base := artifactBase(bin, workload, target, scenario, concurrency, 1, "cold", "compatibility")
	artifact := filepath.Join(r.outDir, "compat", "raw", base+".json")
	cacheDir := r.cacheDir(bin, workload, target, scenario, concurrency, "compatibility")
	stats, err := r.execute(
		bin, workload, target, scenario, concurrency, 1, "cold", "compatibility", cacheDir, artifact,
		compatibilityOutputArgs(artifact)...,
	)
	if err != nil {
		return diagnostics.Input{}, err
	}

	after, err := changes.Capture(workload.Root)
	if err != nil {
		return diagnostics.Input{}, err
	}
	changeSet := changes.Diff(baseline, after)
	input := compatibilityInput(artifact, workload.Root, stats, "", false)
	input.Changes = &changeSet
	input.WorktreePath = workload.Root
	if len(scenario.ExpectedFiles) > 0 {
		expectedMatch, matchErr := expectedFilesMatch(workload.Root, expectedContent)
		if matchErr != nil {
			return diagnostics.Input{}, matchErr
		}
		input.ExpectedMatch = &expectedMatch
	}

	return input, nil
}

func (r *runner) createCompatibilityWorktree(
	workload *preparedWorkload,
	caseName string,
	label string,
) (*preparedWorkload, error) {
	root := filepath.Join(r.outDir, "compat", "worktrees", caseName, label)
	if err := os.MkdirAll(filepath.Dir(root), privateDirMode); err != nil {
		return nil, fmt.Errorf("create compatibility worktree parent: %w", err)
	}
	if _, err := commandOutput(
		r.ctx, "git", "-C", workload.Root, "worktree", "add", "--detach", root, workload.Revision,
	); err != nil {
		return nil, fmt.Errorf("create %s compatibility worktree: %w", label, err)
	}

	isolated := *workload
	isolated.Root = root
	isolated.Dirty = false
	configPath, configHash, err := resolveConfig(root, workload.Config)
	if err != nil {
		cleanupErr := r.removeCompatibilityWorktree(workload.Root, root)
		return nil, errors.Join(err, cleanupErr)
	}
	isolated.ConfigPath = configPath
	isolated.ConfigHash = configHash

	commit, err := commandOutput(r.ctx, "git", "-C", root, "rev-parse", "HEAD")
	if err != nil {
		cleanupErr := r.removeCompatibilityWorktree(workload.Root, root)
		return nil, errors.Join(err, cleanupErr)
	}
	if commit != workload.Revision {
		cleanupErr := r.removeCompatibilityWorktree(workload.Root, root)
		return nil, errors.Join(
			fmt.Errorf("compatibility worktree is at %s, expected %s", commit, workload.Revision), cleanupErr,
		)
	}
	dirty, err := isWorkloadDirty(r.ctx, root)
	if err != nil || dirty {
		cleanupErr := r.removeCompatibilityWorktree(workload.Root, root)
		if err != nil {
			return nil, errors.Join(err, cleanupErr)
		}
		return nil, errors.Join(fmt.Errorf("compatibility worktree is dirty: %s", root), cleanupErr)
	}

	return &isolated, nil
}

func (r *runner) removeCompatibilityWorktree(repository, worktree string) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(r.ctx), compatibilityCleanupTimeout)
	defer cancel()
	_, err := commandOutput(cleanupCtx, "git", "-C", repository, "worktree", "remove", "--force", worktree)
	if err != nil {
		return fmt.Errorf("remove compatibility worktree %s: %w", worktree, err)
	}

	return nil
}

func (r *runner) removeCompatibilityWorktrees(repository string, worktrees []*preparedWorkload) error {
	var result error
	for i := len(worktrees) - 1; i >= 0; i-- {
		result = errors.Join(result, r.removeCompatibilityWorktree(repository, worktrees[i].Root))
	}

	return result
}

func loadExpectedFiles(root string, expectedFiles map[string]string) (map[string][]byte, error) {
	result := make(map[string][]byte, len(expectedFiles))
	for actual, expected := range expectedFiles {
		expectedPath, err := safeJoin(root, expected)
		if err != nil {
			return nil, fmt.Errorf("resolve expected file %s: %w", expected, err)
		}
		expectedData, err := os.ReadFile(expectedPath)
		if err != nil {
			return nil, fmt.Errorf("read expected file %s: %w", expected, err)
		}
		result[actual] = expectedData
	}

	return result, nil
}

func expectedFilesMatch(actualRoot string, expectedFiles map[string][]byte) (bool, error) {
	for actual, expectedData := range expectedFiles {
		if err := validateRelativePath(actual); err != nil {
			return false, fmt.Errorf("resolve fixed file %s: %w", actual, err)
		}
		actualPath := filepath.Join(actualRoot, filepath.Clean(actual))
		actualData, err := os.ReadFile(actualPath)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("read fixed file %s: %w", actual, err)
		}
		if !bytes.Equal(actualData, expectedData) {
			return false, nil
		}
	}

	return true, nil
}

func compatibilityOutputArgs(artifact string) []string {
	return []string{
		"--issues-exit-code=1",
		"--show-stats=false",
		"--max-same-issues=0",
		"--max-issues-per-linter=0",
		"--output.json.path=" + artifact,
	}
}

func (r *runner) runProfiles() error {
	profiles := []struct {
		purpose string
		flag    string
		ext     string
		env     []string
	}{
		{purpose: "cpu-profile", flag: "--cpu-profile-path", ext: "cpu.pprof"},
		{purpose: "heap-profile", flag: "--mem-profile-path", ext: "heap.pprof", env: []string{"GL_MEM_PROFILE_RATE=65536"}},
		{purpose: "trace", flag: "--trace-path", ext: "trace"},
	}

	for i := range r.workloads {
		workload := &r.workloads[i]
		target := workload.Targets[0]
		if workload.ProfileModule != "" {
			target = workload.ProfileModule
		}
		if !slices.Contains(workload.Targets, target) {
			return fmt.Errorf("workload %q profile module %q is not in modules", workload.Name, target)
		}
		for scenarioIndex := range r.scenarios {
			scenario := &r.scenarios[scenarioIndex]
			for _, bin := range r.binaries {
				for _, profile := range profiles {
					base := artifactBase(bin, workload, target, scenario, r.opts.ProfileConcurrency, 1, "cold", profile.purpose)
					artifact := filepath.Join(r.outDir, "profiles", base+"."+profile.ext)
					cacheDir := r.cacheDir(bin, workload, target, scenario, r.opts.ProfileConcurrency, profile.purpose)
					extra := append(slices.Clone(profile.env), profile.flag+"="+artifact)
					if _, err := r.execute(
						bin, workload, target, scenario, r.opts.ProfileConcurrency,
						1, "cold", profile.purpose, cacheDir, artifact, extra...,
					); err != nil {
						return err
					}
				}
			}
		}
	}

	return nil
}

func (r *runner) cacheDir(
	bin binary,
	workload *preparedWorkload,
	target string,
	scenario *scenario,
	concurrency int,
	key string,
) string {
	return filepath.Join(
		r.outDir, "cache", safeName(bin.Label), safeName(workload.Name), safeName(target),
		safeName(scenario.Name), fmt.Sprintf("j%d", concurrency), safeName(key),
	)
}

func (r *runner) execute(
	bin binary,
	workload *preparedWorkload,
	target string,
	scenario *scenario,
	concurrency int,
	iteration int,
	cacheMode string,
	purpose string,
	cacheDir string,
	artifact string,
	extra ...string,
) (*executionStats, error) {
	return r.executeControlled(
		bin, workload, target, scenario, concurrency, iteration, cacheMode, purpose, cacheDir, artifact,
		executionControl{}, extra...,
	)
}

func (r *runner) executeControlled(
	bin binary,
	workload *preparedWorkload,
	target string,
	scenario *scenario,
	concurrency int,
	iteration int,
	cacheMode string,
	purpose string,
	cacheDir string,
	artifact string,
	control executionControl,
	extra ...string,
) (stats *executionStats, runErr error) {
	readySent := false
	defer func() {
		if control.ready != nil && !readySent {
			control.ready <- runErr
		}
	}()
	if err := os.MkdirAll(cacheDir, privateDirMode); err != nil {
		return nil, fmt.Errorf("create cache directory: %w", err)
	}
	workDir, err := resolveWorkDir(workload.Root, target, scenario.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("resolve workload target: %w", err)
	}
	args, err := buildRunArgs(workload, scenario, concurrency, r.opts.RunTimeout, extra)
	if err != nil {
		return nil, err
	}

	base := artifactBase(bin, workload, target, scenario, concurrency, iteration, cacheMode, purpose)
	logPath := filepath.Join(r.outDir, "logs", base+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, privateFileMode)
	if err != nil {
		return nil, fmt.Errorf("create benchmark log: %w", err)
	}
	defer logFile.Close()

	cacheBefore, err := directorySize(cacheDir)
	if err != nil {
		return nil, err
	}

	runCtx, cancel := context.WithTimeout(r.ctx, r.opts.RunTimeout)
	defer cancel()
	cmd := r.newBenchmarkCommand(bin, workDir, cacheDir, args, extra, logFile)
	control.marker = bytesSHA256([]byte(r.outDir + "\x00" + base))
	cmd.Env = replaceEnv(cmd.Env, benchmarkRunMarkerEnv+"="+control.marker)
	if control.ready != nil {
		control.ready <- nil
		readySent = true
	}
	stats, err = r.runControlledCommand(runCtx, cancel, cmd, control)
	if err != nil {
		return nil, err
	}
	cacheAfter, err := directorySize(cacheDir)
	if err != nil {
		return nil, err
	}
	stats.cacheBefore = cacheBefore
	stats.cacheAfter = cacheAfter
	stats.logPath = logPath
	stats.artifactPath = artifact
	stats.args = args

	if err := r.recordExecution(bin, workload, target, scenario, concurrency, iteration, cacheMode, purpose, stats); err != nil {
		return nil, err
	}

	return stats, nil
}

func (r *runner) runControlledCommand(
	runCtx context.Context,
	cancel context.CancelFunc,
	cmd *exec.Cmd,
	control executionControl,
) (*executionStats, error) {
	if control.start != nil {
		select {
		case <-control.start:
		case <-runCtx.Done():
			return nil, fmt.Errorf("wait for benchmark start: %w", runCtx.Err())
		}
	}

	startedAt := time.Now().UTC()
	started := time.Now()
	finished, terminated, rootPID, startErr := startCommand(runCtx, cmd)
	if startErr != nil {
		return nil, startErr
	}
	cancelledAt := scheduleCancellation(cancel, finished, control.cancelAfter)
	stopMemory := make(chan struct{})
	peakMemory := trackPeakTreeRSS(
		runCtx,
		rootPID,
		r.opts.MaxRSSMiB*bytesPerMiB,
		cancel,
		stopMemory,
	)
	waitErr := cmd.Wait()
	waitFinished := time.Now()
	close(finished)
	cancelStarted := <-cancelledAt
	termination := <-terminated
	close(stopMemory)
	rssStats := <-peakMemory
	wall := time.Since(started)
	terminationReason := ""
	if rssStats.exceeded {
		terminationReason = "rss_limit"
	} else if !cancelStarted.IsZero() {
		terminationReason = compatibilityModeCancel
	} else if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		terminationReason = "timeout"
	}
	cancellation := time.Duration(0)
	if !cancelStarted.IsZero() {
		cancellation = waitFinished.Sub(cancelStarted)
	}
	for _, pid := range termination.pids {
		rssStats.observed[pid] = struct{}{}
	}
	markedProcesses, err := terminateResidualProcesses(control.marker)
	if err != nil {
		return nil, err
	}
	orphanedProcesses := termination.orphaned + markedProcesses
	survivingProcesses := orphanedProcesses + waitForProcessExit(rssStats.observed)

	exitCode, err := commandExitCode(waitErr)
	if err != nil {
		return nil, err
	}

	var userCPU, systemCPU time.Duration
	if cmd.ProcessState != nil {
		userCPU = cmd.ProcessState.UserTime()
		systemCPU = cmd.ProcessState.SystemTime()
	}
	stats := executionStats{
		startedAt:          startedAt,
		wall:               wall,
		userCPU:            userCPU,
		systemCPU:          systemCPU,
		peakRSS:            rssStats.peak,
		exitCode:           exitCode,
		terminationReason:  terminationReason,
		cancellation:       cancellation,
		survivingProcesses: survivingProcesses,
	}

	return &stats, nil
}

func (r *runner) recordExecution(
	bin binary,
	workload *preparedWorkload,
	target string,
	scenario *scenario,
	concurrency int,
	iteration int,
	cacheMode string,
	purpose string,
	stats *executionStats,
) error {
	record := result{
		SchemaVersion:      schemaVersion,
		Binary:             bin.Label,
		Workload:           workload.Name,
		WorkloadRevision:   workload.Revision,
		Target:             target,
		Scenario:           scenario.Name,
		CacheMode:          cacheMode,
		Concurrency:        concurrency,
		Iteration:          iteration,
		Purpose:            purpose,
		StartedAt:          stats.startedAt,
		WallNS:             stats.wall.Nanoseconds(),
		UserCPUNS:          stats.userCPU.Nanoseconds(),
		SystemCPUNS:        stats.systemCPU.Nanoseconds(),
		PeakTreeRSSBytes:   stats.peakRSS,
		CacheBytesBefore:   stats.cacheBefore,
		CacheBytesAfter:    stats.cacheAfter,
		ExitCode:           stats.exitCode,
		LogPath:            relativePath(r.outDir, stats.logPath),
		ArtifactPath:       relativePath(r.outDir, stats.artifactPath),
		TerminationReason:  stats.terminationReason,
		CancellationNS:     stats.cancellation.Nanoseconds(),
		SurvivingProcesses: stats.survivingProcesses,
		Command:            append([]string{bin.Path}, stats.args...),
	}
	r.resultsMu.Lock()
	defer r.resultsMu.Unlock()
	if err := appendJSONLine(r.results, record); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(os.Stdout, "%s/%s/%s/%s j%d %s %s: %s, %d MiB\n",
		bin.Label, workload.Name, safeName(target), scenario.Name, concurrency, cacheMode, purpose,
		stats.wall.Round(time.Millisecond), stats.peakRSS/bytesPerMiB)

	compatibilityRun := strings.HasPrefix(purpose, "compatibility")
	if stats.terminationReason != "" && purpose != "compatibility-cancel" {
		return fmt.Errorf("benchmark command stopped by %s; see %s", stats.terminationReason, stats.logPath)
	}
	if stats.exitCode != 0 && !compatibilityRun {
		return fmt.Errorf("benchmark command exited with %d; see %s", stats.exitCode, stats.logPath)
	}

	return nil
}

func buildRunArgs(
	workload *preparedWorkload,
	scenario *scenario,
	concurrency int,
	runTimeout time.Duration,
	extra []string,
) ([]string, error) {
	cliTimeout := runTimeout
	if scenario.Mode == compatibilityModeTimeout {
		cliTimeout = compatibilityCLITimeout
	}
	runnerMode := "--allow-serial-runners"
	if scenario.Mode == compatibilityModeParallel {
		runnerMode = "--allow-parallel-runners"
	}
	args := []string{
		"--color=never",
		"run",
		"-v",
		"--timeout=" + cliTimeout.String(),
		runnerMode,
		fmt.Sprintf("--fix=%t", scenario.Mutates),
		fmt.Sprintf("--concurrency=%d", concurrency),
	}
	if !slices.ContainsFunc(extra, func(value string) bool {
		return strings.HasPrefix(value, "--issues-exit-code=")
	}) {
		args = append(args, "--issues-exit-code=0")
	}
	if scenario.UseConfig {
		if workload.ConfigPath == "" {
			return nil, fmt.Errorf("scenario %q requires config for workload %q", scenario.Name, workload.Name)
		}
		args = append(args, "--config="+workload.ConfigPath)
	}
	if workload.Tests != nil {
		args = append(args, fmt.Sprintf("--tests=%t", *workload.Tests))
	}
	args = append(args, scenario.Args...)
	for _, value := range extra {
		if strings.HasPrefix(value, "--") {
			args = append(args, value)
		}
	}
	packages := scenario.Packages
	if len(packages) == 0 {
		packages = workload.Packages
	}
	if len(packages) == 0 {
		packages = []string{"./..."}
	}

	return append(args, packages...), nil
}

func resolveWorkDir(root, target, scenarioDir string) (string, error) {
	workDir, err := safeJoin(root, target)
	if err != nil {
		return "", err
	}
	if scenarioDir == "" {
		return workDir, nil
	}

	return safeJoin(workDir, scenarioDir)
}

func (r *runner) newBenchmarkCommand(
	bin binary,
	workDir string,
	cacheDir string,
	args []string,
	extra []string,
	output io.Writer,
) *exec.Cmd {
	environ := replaceEnv(os.Environ(),
		"GOLANGCI_LINT_CACHE="+cacheDir,
		fmt.Sprintf("GOMAXPROCS=%d", r.opts.GoMaxProcs),
		fmt.Sprintf("GOMEMLIMIT=%dMiB", r.opts.MaxRSSMiB),
		fmt.Sprintf("GOFLAGS=-p=%d", r.opts.GoMaxProcs),
		"GOROOT="+r.goRoot,
		"PATH="+filepath.Join(r.goRoot, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	for _, value := range extra {
		if !strings.HasPrefix(value, "--") {
			environ = replaceEnv(environ, value)
		}
	}

	path := bin.Path
	commandArgs := append([]string{bin.Path}, args...)
	if runtime.GOOS != "windows" && r.opts.Nice > 0 {
		path = "/usr/bin/nice"
		commandArgs = append([]string{path, "-n", strconv.Itoa(r.opts.Nice), bin.Path}, args...)
	}

	return &exec.Cmd{
		Path:   path,
		Args:   commandArgs,
		Dir:    workDir,
		Env:    environ,
		Stdout: output,
		Stderr: output,
	}
}

func startCommand(
	ctx context.Context,
	cmd *exec.Cmd,
) (finished chan struct{}, terminated <-chan processTermination, rootPID int32, startErr error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, nil, 0, fmt.Errorf("start benchmark command: %w", ctxErr)
	}
	configureProcessTree(cmd)
	if cmdErr := cmd.Start(); cmdErr != nil {
		return nil, nil, 0, fmt.Errorf("start benchmark command: %w", cmdErr)
	}
	rootPID, startErr = checkedPID(cmd.Process.Pid)
	if startErr != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()

		return nil, nil, 0, startErr
	}

	finished = make(chan struct{})
	terminatedCh := make(chan processTermination, 1)
	go func() {
		select {
		case <-ctx.Done():
			terminatedCh <- processTermination{pids: terminateProcessTree(rootPID)}
		case <-finished:
			pids := terminateProcessTree(rootPID)
			terminatedCh <- processTermination{pids: pids, orphaned: len(pids)}
		}
	}()

	return finished, terminatedCh, rootPID, nil
}

func scheduleCancellation(cancel context.CancelFunc, finished <-chan struct{}, delay time.Duration) <-chan time.Time {
	result := make(chan time.Time, 1)
	if delay <= 0 {
		result <- time.Time{}
		return result
	}
	go func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case started := <-timer.C:
			cancel()
			result <- started
		case <-finished:
			result <- time.Time{}
		}
	}()

	return result
}

func checkedPID(pid int) (int32, error) {
	if pid <= 0 || pid > math.MaxInt32 {
		return 0, fmt.Errorf("benchmark PID exceeds supported range: %d", pid)
	}

	return int32(pid), nil
}

func commandExitCode(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return 0, fmt.Errorf("wait for benchmark command: %w", err)
	}

	return exitErr.ExitCode(), nil
}

func artifactBase(
	bin binary,
	workload *preparedWorkload,
	target string,
	scenario *scenario,
	concurrency int,
	iteration int,
	cacheMode string,
	purpose string,
) string {
	return strings.Join([]string{
		safeName(bin.Label),
		safeName(workload.Name),
		safeName(target),
		safeName(scenario.Name),
		fmt.Sprintf("j%d", concurrency),
		fmt.Sprintf("i%d", iteration),
		safeName(cacheMode),
		safeName(purpose),
	}, "-")
}

func compatibilityCaseBase(
	workload *preparedWorkload,
	target string,
	scenario *scenario,
	concurrency int,
) string {
	return strings.Join([]string{
		safeName(workload.Name),
		safeName(target),
		safeName(scenario.Name),
		fmt.Sprintf("j%d", concurrency),
	}, "-")
}

func safeName(value string) string {
	var b strings.Builder
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || strings.ContainsRune("._-", char) {
			b.WriteRune(char)
			continue
		}
		b.WriteByte('_')
	}

	return b.String()
}

func replaceEnv(environ []string, values ...string) []string {
	overrides := make(map[string]string, len(values))
	for _, value := range values {
		key, _, _ := strings.Cut(value, "=")
		overrides[key] = value
	}

	result := make([]string, 0, len(environ)+len(overrides))
	for _, value := range environ {
		key, _, _ := strings.Cut(value, "=")
		if _, ok := overrides[key]; ok {
			continue
		}
		result = append(result, value)
	}
	result = append(result, values...)

	return result
}

func trackPeakTreeRSS(
	ctx context.Context,
	rootPID int32,
	maxRSS uint64,
	cancel context.CancelFunc,
	stop <-chan struct{},
) <-chan memoryStats {
	resultCh := make(chan memoryStats, 1)
	go func() {
		defer close(resultCh)

		var peak uint64
		observed := make(map[int32]struct{})
		ticker := time.NewTicker(rssSampleInterval)
		defer ticker.Stop()

		for {
			seen := make(map[int32]struct{})
			current := processTreeRSS(ctx, rootPID, seen)
			for pid := range seen {
				observed[pid] = struct{}{}
			}
			if current > peak {
				peak = current
			}
			if current > maxRSS {
				resultCh <- memoryStats{peak: peak, exceeded: true, observed: observed}
				cancel()

				return
			}
			select {
			case <-ctx.Done():
				resultCh <- memoryStats{peak: peak, observed: observed}
				return
			case <-stop:
				resultCh <- memoryStats{peak: peak, observed: observed}
				return
			case <-ticker.C:
			}
		}
	}()

	return resultCh
}

func waitForProcessExit(pids map[int32]struct{}) int {
	deadline := time.Now().Add(processExitWait)
	for {
		alive := countRunningProcesses(pids)
		if alive == 0 || time.Now().After(deadline) {
			return alive
		}
		time.Sleep(processExitPoll)
	}
}

func countRunningProcesses(pids map[int32]struct{}) int {
	count := 0
	for pid := range pids {
		p, err := process.NewProcess(pid)
		if err != nil {
			continue
		}
		running, err := p.IsRunning()
		if err == nil && running {
			count++
		}
	}

	return count
}

func processTreeRSS(ctx context.Context, pid int32, seen map[int32]struct{}) uint64 {
	if _, ok := seen[pid]; ok {
		return 0
	}
	seen[pid] = struct{}{}

	p, err := process.NewProcess(pid)
	if err != nil {
		return 0
	}
	info, err := p.MemoryInfoWithContext(ctx)
	if err != nil {
		return 0
	}
	total := info.RSS
	children, err := p.ChildrenWithContext(ctx)
	if err != nil {
		return total
	}
	for _, child := range children {
		total += processTreeRSS(ctx, child.Pid, seen)
	}

	return total
}

func directorySize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if path != root && errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		total += info.Size()

		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("measure directory %s: %w", root, err)
	}

	return total, nil
}

func writeJSON(path string, value any) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, privateFileMode)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}

	return nil
}

func appendJSONLine(file *os.File, value any) error {
	writer := bufio.NewWriter(file)
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		return fmt.Errorf("encode result: %w", err)
	}
	if err := writer.Flush(); err != nil {
		return fmt.Errorf("flush result: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync result: %w", err)
	}

	return nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}

	return hex.EncodeToString(hash.Sum(nil)), nil
}

func bytesSHA256(value []byte) string {
	sum := sha256.Sum256(value)

	return hex.EncodeToString(sum[:])
}

func commandOutput(ctx context.Context, name string, args ...string) (string, error) {
	output, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("run %s: %w: %s", name, err, strings.TrimSpace(string(output)))
	}

	return strings.TrimSpace(string(output)), nil
}

func runCommand(ctx context.Context, dir string, output io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir

	var captured bytes.Buffer
	destination := io.Writer(&captured)
	if output != nil {
		destination = io.MultiWriter(output, &captured)
	}
	cmd.Stdout = destination
	cmd.Stderr = destination
	if err := cmd.Run(); err != nil {
		details := strings.TrimSpace(captured.String())
		if details == "" {
			return fmt.Errorf("run %s: %w", strings.Join(cmd.Args, " "), err)
		}

		return fmt.Errorf("run %s: %w: %s", strings.Join(cmd.Args, " "), err, details)
	}

	return nil
}

func relativePath(root, path string) string {
	if path == "" {
		return ""
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}

	return rel
}
