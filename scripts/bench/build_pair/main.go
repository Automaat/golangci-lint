package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	schemaVersion         = 2
	defaultBuildTimeout   = 5 * time.Minute
	defaultMaxMemoryMiB   = 1024
	defaultGoMaxProcs     = 2
	defaultNice           = 10
	privateDirMode        = 0o750
	privateFileMode       = 0o600
	bytesPerMiB           = 1024 * 1024
	shortCommitSHALength  = 12
	baselineModeMergeBase = "merge-base"
	baselineModeExact     = "exact"
)

type options struct {
	CandidateRef string
	UpstreamRef  string
	BaselineMode string
	OutputDir    string
	BuildTimeout time.Duration
	MaxMemoryMiB uint64
	GoMaxProcs   int
	Nice         int
}

type metadata struct {
	SchemaVersion int              `json:"schema_version"`
	CreatedAt     time.Time        `json:"created_at"`
	Repository    string           `json:"repository"`
	GoVersion     string           `json:"go_version"`
	CandidateRef  string           `json:"candidate_ref"`
	CandidateSHA  string           `json:"candidate_sha"`
	UpstreamRef   string           `json:"upstream_ref"`
	UpstreamSHA   string           `json:"upstream_sha"`
	MergeBaseSHA  string           `json:"merge_base_sha"`
	BaselineMode  string           `json:"baseline_mode"`
	BaselineSHA   string           `json:"baseline_sha"`
	Limits        limitsMetadata   `json:"limits"`
	Binaries      []binaryMetadata `json:"binaries"`
}

type limitsMetadata struct {
	BuildTimeoutNS       int64  `json:"build_timeout_ns"`
	MaxMemoryBytes       uint64 `json:"max_memory_bytes"`
	GoMaxProcs           int    `json:"go_max_procs"`
	GoBuildParallelism   int    `json:"go_build_parallelism"`
	UnixSchedulingNicety int    `json:"unix_scheduling_nicety"`
}

type binaryMetadata struct {
	Label      string `json:"label"`
	Path       string `json:"path"`
	Commit     string `json:"commit"`
	CommitDate string `json:"commit_date"`
	SHA256     string `json:"sha256"`
}

type buildTarget struct {
	label      string
	commit     string
	commitDate string
	path       string
}

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "build benchmark pair: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	opts, err := parseOptions(args)
	if err != nil {
		return err
	}

	repository, err := commandOutput(ctx, "", nil, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	candidateSHA, err := resolveRef(ctx, repository, opts.CandidateRef)
	if err != nil {
		return err
	}
	upstreamSHA, err := resolveRef(ctx, repository, opts.UpstreamRef)
	if err != nil {
		return err
	}
	mergeBaseSHA, err := commandOutput(ctx, repository, nil, "git", "merge-base", candidateSHA, upstreamSHA)
	if err != nil {
		return err
	}
	goVersion, err := commandOutput(ctx, repository, nil, "go", "version")
	if err != nil {
		return err
	}

	outputDir, err := filepath.Abs(opts.OutputDir)
	if err != nil {
		return fmt.Errorf("resolve output directory: %w", err)
	}
	err = os.MkdirAll(outputDir, privateDirMode)
	if err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}

	baselineSHA := selectBaselineSHA(opts.BaselineMode, mergeBaseSHA, upstreamSHA)
	pairs, err := prepareBuildTargets(ctx, repository, outputDir, baselineSHA, candidateSHA)
	if err != nil {
		return err
	}
	value := metadata{
		SchemaVersion: schemaVersion,
		CreatedAt:     time.Now().UTC(),
		Repository:    repository,
		GoVersion:     goVersion,
		CandidateRef:  opts.CandidateRef,
		CandidateSHA:  candidateSHA,
		UpstreamRef:   opts.UpstreamRef,
		UpstreamSHA:   upstreamSHA,
		MergeBaseSHA:  mergeBaseSHA,
		BaselineMode:  opts.BaselineMode,
		BaselineSHA:   baselineSHA,
		Limits: limitsMetadata{
			BuildTimeoutNS:       opts.BuildTimeout.Nanoseconds(),
			MaxMemoryBytes:       opts.MaxMemoryMiB * bytesPerMiB,
			GoMaxProcs:           opts.GoMaxProcs,
			GoBuildParallelism:   opts.GoMaxProcs,
			UnixSchedulingNicety: opts.Nice,
		},
	}
	for _, pair := range pairs {
		if err := buildCommit(ctx, repository, pair.commit, pair.commitDate, pair.path, &opts); err != nil {
			return fmt.Errorf("build %s: %w", pair.label, err)
		}
		hash, err := fileSHA256(pair.path)
		if err != nil {
			return err
		}
		value.Binaries = append(value.Binaries, binaryMetadata{
			Label:      pair.label,
			Path:       pair.path,
			Commit:     pair.commit,
			CommitDate: pair.commitDate,
			SHA256:     hash,
		})
	}
	if err := writeJSON(filepath.Join(outputDir, "pair.json"), value); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(os.Stdout, "benchmark pair: upstream %s, fork %s\n",
		baselineSHA[:shortCommitSHALength], candidateSHA[:shortCommitSHALength])
	_, _ = fmt.Fprintf(os.Stdout, "pair artifacts: %s\n", outputDir)

	return nil
}

func selectBaselineSHA(mode, mergeBaseSHA, upstreamSHA string) string {
	if mode == baselineModeExact {
		return upstreamSHA
	}
	return mergeBaseSHA
}

func parseOptions(args []string) (options, error) {
	var opts options

	fs := flag.NewFlagSet("build_pair", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&opts.CandidateRef, "candidate-ref", "HEAD", "candidate Git ref")
	fs.StringVar(&opts.UpstreamRef, "upstream-ref", "upstream/main", "upstream Git ref")
	fs.StringVar(&opts.BaselineMode, "baseline-mode", baselineModeMergeBase,
		"baseline commit: merge-base or exact upstream ref")
	fs.StringVar(&opts.OutputDir, "out-dir", "dist/bench/bin", "binary and metadata output directory")
	fs.DurationVar(&opts.BuildTimeout, "build-timeout", defaultBuildTimeout, "hard timeout for each build")
	fs.Uint64Var(&opts.MaxMemoryMiB, "max-memory-mib", defaultMaxMemoryMiB, "Go build memory limit")
	fs.IntVar(&opts.GoMaxProcs, "go-max-procs", defaultGoMaxProcs, "GOMAXPROCS and Go build parallelism")
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
	if opts.CandidateRef == "" {
		return errors.New("--candidate-ref is required")
	}
	if opts.UpstreamRef == "" {
		return errors.New("--upstream-ref is required")
	}
	if opts.BaselineMode != baselineModeMergeBase && opts.BaselineMode != baselineModeExact {
		return fmt.Errorf("invalid --baseline-mode %q", opts.BaselineMode)
	}
	if opts.OutputDir == "" {
		return errors.New("--out-dir is required")
	}
	if opts.BuildTimeout <= 0 {
		return errors.New("--build-timeout must be positive")
	}
	if opts.MaxMemoryMiB == 0 || opts.MaxMemoryMiB > math.MaxUint64/bytesPerMiB {
		return errors.New("--max-memory-mib must be positive and representable")
	}
	if opts.GoMaxProcs < 1 {
		return errors.New("--go-max-procs must be positive")
	}
	if opts.Nice < 0 || opts.Nice > 20 {
		return errors.New("--nice must be between 0 and 20")
	}

	return nil
}

func resolveRef(ctx context.Context, repository, ref string) (string, error) {
	sha, err := commandOutput(ctx, repository, nil, "git", "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve ref %q: %w", ref, err)
	}

	return sha, nil
}

func prepareBuildTargets(
	ctx context.Context,
	repository, outputDir, mergeBaseSHA, candidateSHA string,
) ([]buildTarget, error) {
	targets := []buildTarget{
		{label: "upstream", commit: mergeBaseSHA, path: filepath.Join(outputDir, binaryName("upstream"))},
		{label: "fork", commit: candidateSHA, path: filepath.Join(outputDir, binaryName("fork"))},
	}
	for i := range targets {
		date, err := commandOutput(ctx, repository, nil, "git", "show", "-s", "--format=%cI", targets[i].commit)
		if err != nil {
			return nil, fmt.Errorf("read %s commit date: %w", targets[i].label, err)
		}
		targets[i].commitDate = date
	}

	return targets, nil
}

func buildCommit(ctx context.Context, repository, commit, commitDate, outputPath string, opts *options) error {
	tempRoot, err := os.MkdirTemp("", "golangci-lint-build-pair-")
	if err != nil {
		return fmt.Errorf("create temporary directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(tempRoot) }()

	worktree := filepath.Join(tempRoot, "worktree")
	if _, err := commandOutput(ctx, repository, nil, "git", "worktree", "add", "--detach", worktree, commit); err != nil {
		return fmt.Errorf("create worktree: %w", err)
	}

	buildCtx, cancel := context.WithTimeout(ctx, opts.BuildTimeout)
	defer cancel()
	buildErr := runBuild(buildCtx, worktree, commit, commitDate, outputPath, opts)
	_, cleanupErr := commandOutput(ctx, repository, nil, "git", "worktree", "remove", "--force", worktree)

	return errors.Join(buildErr, cleanupErr)
}

func runBuild(ctx context.Context, worktree, commit, commitDate, outputPath string, opts *options) error {
	environ := replaceEnv(os.Environ(),
		fmt.Sprintf("GOMAXPROCS=%d", opts.GoMaxProcs),
		fmt.Sprintf("GOMEMLIMIT=%dMiB", opts.MaxMemoryMiB),
		fmt.Sprintf("GOFLAGS=-p=%d", opts.GoMaxProcs),
	)
	name := "go"
	linkerFlags := fmt.Sprintf("-s -w -X main.version=benchmark -X main.commit=%s -X main.date=%s", commit, commitDate)
	args := []string{"build", "-buildvcs=false", "-trimpath", "-ldflags", linkerFlags, "-o", outputPath, "./cmd/golangci-lint"}
	if runtime.GOOS != "windows" && opts.Nice > 0 {
		name = "/usr/bin/nice"
		args = append([]string{"-n", strconv.Itoa(opts.Nice), "go"}, args...)
	}
	_, err := commandOutput(ctx, worktree, environ, name, args...)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("build timed out after %s", opts.BuildTimeout)
	}
	if err != nil {
		return err
	}

	return nil
}

func commandOutput(ctx context.Context, dir string, environ []string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if environ != nil {
		cmd.Env = environ
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("run %s: %w: %s", name, err, strings.TrimSpace(string(output)))
	}

	return strings.TrimSpace(string(output)), nil
}

func binaryName(label string) string {
	if runtime.GOOS == "windows" {
		return label + ".exe"
	}

	return label
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
	return append(result, values...)
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open binary for hashing: %w", err)
	}
	defer file.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("hash binary: %w", err)
	}

	return hex.EncodeToString(hash.Sum(nil)), nil
}

func writeJSON(path string, value any) error {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return err
	}
	if err := os.WriteFile(path, buffer.Bytes(), privateFileMode); err != nil {
		return fmt.Errorf("write pair metadata: %w", err)
	}

	return nil
}
