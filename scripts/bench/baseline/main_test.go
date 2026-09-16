package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestStartCommandStopsAtTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires /bin/sleep")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sleep", "30")
	finished, terminated, _, err := startCommand(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("expected timed out command to fail")
	}
	close(finished)
	termination := <-terminated
	observed := make(map[int32]struct{}, len(termination.pids))
	for _, pid := range termination.pids {
		observed[pid] = struct{}{}
	}
	if alive := waitForProcessExit(observed); alive != 0 {
		t.Fatalf("expected no surviving processes, got %d", alive)
	}
}

func TestTrackPeakTreeRSSStopsAtLimit(t *testing.T) {
	pid := os.Getpid()
	rootPID, err := checkedPID(pid)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := make(chan struct{})
	statsCh := trackPeakTreeRSS(ctx, rootPID, bytesPerMiB, cancel, stop)
	stats := <-statsCh
	close(stop)
	if !stats.exceeded {
		t.Fatalf("expected RSS limit to be exceeded, peak was %d bytes", stats.peak)
	}
}

func TestStartCommandStopsUnsampledOrphan(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires /bin/sh")
	}
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", "sleep 30 &")
	finished, terminated, _, err := startCommand(t.Context(), cmd)
	require.NoError(t, err)
	require.NoError(t, cmd.Wait())
	close(finished)
	termination := <-terminated
	require.Positive(t, termination.orphaned)
	observed := make(map[int32]struct{}, len(termination.pids))
	for _, pid := range termination.pids {
		observed[pid] = struct{}{}
	}
	require.Zero(t, waitForProcessExit(observed))
}

func TestParseOptionsSafetyDefaults(t *testing.T) {
	opts, err := parseOptions([]string{"--fork-bin", "fork"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.RunTimeout != defaultRunTimeout || opts.MaxRSSMiB != defaultMaxRSSMiB ||
		opts.GoMaxProcs != defaultGoMaxProcs || opts.Nice != defaultNice {
		t.Fatalf("unexpected safety defaults: %+v", opts)
	}
}

func TestParseOptionsRejectsUnsafeLimits(t *testing.T) {
	for _, args := range [][]string{
		{"--fork-bin", "fork", "--run-timeout", "0s"},
		{"--fork-bin", "fork", "--max-rss-mib", "0"},
		{"--fork-bin", "fork", "--go-max-procs", "0"},
		{"--fork-bin", "fork", "--nice", "21"},
	} {
		if _, err := parseOptions(args); err == nil {
			t.Fatalf("expected %v to fail", args)
		}
	}
}

func TestParseOptionsCompatibilityRequirements(t *testing.T) {
	valid := []string{
		"--compatibility",
		"--fork-bin", "fork",
		"--upstream-bin", "upstream",
		"--workload", "small",
		"--concurrency", "1",
	}
	if _, err := parseOptions(valid); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{
		{"--compatibility", "--upstream-bin", "upstream", "--workload", "small", "--concurrency", "1"},
		{"--compatibility", "--fork-bin", "fork", "--workload", "small", "--concurrency", "1"},
		{"--compatibility", "--fork-bin", "fork", "--upstream-bin", "upstream", "--concurrency", "1"},
		{"--compatibility", "--fork-bin", "fork", "--upstream-bin", "upstream", "--workload", "small"},
		append(slices.Clone(valid), "--profiles"),
	} {
		if _, err := parseOptions(args); err == nil {
			t.Fatalf("expected %v to fail", args)
		}
	}
}

func TestParsePositiveInts(t *testing.T) {
	actual, err := parsePositiveInts("1, 2,4,2")
	if err != nil {
		t.Fatal(err)
	}
	expected := []int{1, 2, 4}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("expected %v, got %v", expected, actual)
	}
}

func TestParsePositiveIntsRejectsInvalidValues(t *testing.T) {
	for _, value := range []string{"", "0", "1,nope"} {
		if _, err := parsePositiveInts(value); err == nil {
			t.Fatalf("expected %q to fail", value)
		}
	}
}

func TestParseCacheModes(t *testing.T) {
	actual, err := parseCacheModes("warm,cold,warm")
	if err != nil {
		t.Fatal(err)
	}
	expected := []string{"warm", "cold"}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("expected %v, got %v", expected, actual)
	}
}

func TestFilterTargets(t *testing.T) {
	actual := filterTargets([]string{".", "scripts/tool"}, "scripts/tool")
	expected := []string{"scripts/tool"}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("expected %v, got %v", expected, actual)
	}
	if actual := filterTargets([]string{"."}, "missing"); actual != nil {
		t.Fatalf("expected no target, got %v", actual)
	}
}

func TestValidateManifest(t *testing.T) {
	m := manifest{
		SchemaVersion: schemaVersion,
		GoVersion:     "go1.26.0",
		Concurrency:   []int{1, 2, 4, 8},
		Runs:          3,
		Workloads: []workload{{
			Name:     "small",
			URL:      "https://example.com/repo.git",
			Revision: "0123456789abcdef0123456789abcdef01234567",
		}},
		Scenarios: []scenario{{Name: "configured", UseConfig: true}},
	}
	if err := validateManifest(&m); err != nil {
		t.Fatal(err)
	}
}

func TestValidateManifestRejectsDuplicateNames(t *testing.T) {
	m := manifest{
		SchemaVersion: schemaVersion,
		GoVersion:     "go1.26.0",
		Concurrency:   []int{1},
		Runs:          1,
		Workloads: []workload{
			{Name: "same", URL: "https://example.com/a.git", Revision: "0123456789abcdef0123456789abcdef01234567"},
			{Name: "same", URL: "https://example.com/b.git", Revision: "0123456789abcdef0123456789abcdef01234567"},
		},
		Scenarios: []scenario{{Name: "configured"}},
	}
	if err := validateManifest(&m); err == nil {
		t.Fatal("expected duplicate workload names to fail")
	}
}

func TestValidateManifestRejectsEscapingScenarioWorkDir(t *testing.T) {
	m := manifest{
		SchemaVersion: schemaVersion,
		GoVersion:     "go1.26.0",
		Concurrency:   []int{1},
		Runs:          1,
		Workloads: []workload{{
			Name:     "small",
			URL:      "https://example.com/repo.git",
			Revision: "0123456789abcdef0123456789abcdef01234567",
		}},
		Scenarios: []scenario{{Name: "escape", WorkDir: "../outside"}},
	}
	if err := validateManifest(&m); err == nil {
		t.Fatal("expected escaping scenario working directory to fail")
	}
}

func TestValidateScenariosRejectsImplicitFixAndUnsafeExpectedFiles(t *testing.T) {
	for _, item := range []scenario{
		{Name: "implicit-fix", Args: []string{"--fix"}},
		{Name: "implicit-timeout", Args: []string{"--timeout=1ms"}},
		{Name: "implicit-runner-mode", Args: []string{"--allow-parallel-runners"}},
		{Name: "unknown-mode", Mode: "unknown"},
		{Name: "mutating-failure", Mode: compatibilityModeCancel, Mutates: true},
		{Name: "expected-without-mutation", ExpectedFiles: map[string]string{"in.go": "out.go"}},
		{Name: "escaping-actual", Mutates: true, ExpectedFiles: map[string]string{"../in.go": "out.go"}},
		{Name: "escaping-golden", Mutates: true, ExpectedFiles: map[string]string{"in.go": "../out.go"}},
	} {
		if err := validateScenarios([]scenario{item}); err == nil {
			t.Fatalf("expected scenario %+v to fail", item)
		}
	}
}

func TestValidateScenariosAcceptsCompatibilityModes(t *testing.T) {
	for _, mode := range []string{
		compatibilityModeTimeout,
		compatibilityModeCancel,
		compatibilityModeParallel,
		compatibilityModeCorrupt,
	} {
		if err := validateScenarios([]scenario{{Name: mode, Mode: mode}}); err != nil {
			t.Fatalf("expected mode %q to pass: %v", mode, err)
		}
	}
}

func TestLoadManifestRejectsTrailingData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(path, []byte(`{} {}`), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, err := loadManifest(path)
	if err == nil || !strings.Contains(err.Error(), "trailing data") {
		t.Fatalf("expected trailing data error, got %v", err)
	}
}

func TestSafeJoin(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "module")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}

	actual, err := safeJoin(root, "module")
	if err != nil {
		t.Fatal(err)
	}
	if actual != dir {
		t.Fatalf("expected %s, got %s", dir, actual)
	}
	if _, err := safeJoin(root, "../escape"); err == nil {
		t.Fatal("expected escaping path to fail")
	}
}

func TestArtifactBase(t *testing.T) {
	actual := artifactBase(
		binary{Label: "fork"},
		&preparedWorkload{workload: workload{Name: "multi"}},
		"scripts/tool",
		&scenario{Name: "configured"},
		4,
		2,
		"cold",
		"timing",
	)
	expected := "fork-multi-scripts_tool-configured-j4-i2-cold-timing"
	if actual != expected {
		t.Fatalf("expected %q, got %q", expected, actual)
	}
}

func TestCompatibilityCaseBase(t *testing.T) {
	actual := compatibilityCaseBase(
		&preparedWorkload{workload: workload{Name: "multi"}},
		"scripts/tool",
		&scenario{Name: "goanalysis"},
		2,
	)
	expected := "multi-scripts_tool-goanalysis-j2"
	if actual != expected {
		t.Fatalf("expected %q, got %q", expected, actual)
	}
}

func TestCompatibilityOutputArgsDisableIssueLimits(t *testing.T) {
	args := compatibilityOutputArgs("issues.json")
	for _, expected := range []string{
		"--max-same-issues=0",
		"--max-issues-per-linter=0",
		"--output.json.path=issues.json",
	} {
		if !slices.Contains(args, expected) {
			t.Fatalf("expected %q in %v", expected, args)
		}
	}
}

func TestReplaceEnv(t *testing.T) {
	actual := replaceEnv([]string{"PATH=old", "KEEP=value", "GOROOT=old"}, "PATH=new", "GOROOT=new")
	expected := []string{"KEEP=value", "PATH=new", "GOROOT=new"}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("expected %v, got %v", expected, actual)
	}
}

func TestBuildRunArgsUsesSafetyTimeout(t *testing.T) {
	tests := false
	args, err := buildRunArgs(
		&preparedWorkload{workload: workload{Tests: &tests}}, &scenario{}, 2, 3*time.Minute, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(args, "--timeout=3m0s") {
		t.Fatalf("expected timeout argument, got %v", args)
	}
	if !slices.Contains(args, "--tests=false") {
		t.Fatalf("expected tests argument, got %v", args)
	}
	if !slices.Contains(args, "--allow-serial-runners") {
		t.Fatalf("expected serial runner lock argument, got %v", args)
	}
}

func TestBuildRunArgsUsesCompatibilityControls(t *testing.T) {
	timeoutArgs, err := buildRunArgs(
		&preparedWorkload{}, &scenario{Mode: compatibilityModeTimeout}, 1, time.Minute, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(timeoutArgs, "--timeout=1ms") {
		t.Fatalf("expected compatibility timeout, got %v", timeoutArgs)
	}

	parallelArgs, err := buildRunArgs(
		&preparedWorkload{}, &scenario{Mode: compatibilityModeParallel}, 1, time.Minute, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(parallelArgs, "--allow-parallel-runners") ||
		slices.Contains(parallelArgs, "--allow-serial-runners") {
		t.Fatalf("expected parallel runner mode, got %v", parallelArgs)
	}
}

func TestBuildRunArgsUsesScenarioPackages(t *testing.T) {
	args, err := buildRunArgs(
		&preparedWorkload{workload: workload{Packages: []string{"./..."}}},
		&scenario{Packages: []string{"testdata/example.go"}},
		1,
		time.Minute,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if args[len(args)-1] != "testdata/example.go" {
		t.Fatalf("expected scenario package, got %v", args)
	}
}

func TestBuildRunArgsEnablesExplicitMutation(t *testing.T) {
	args, err := buildRunArgs(
		&preparedWorkload{}, &scenario{Mutates: true}, 1, time.Minute, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(args, "--fix=true") || slices.Contains(args, "--fix=false") {
		t.Fatalf("expected explicit fix mode, got %v", args)
	}
}

func TestResolveWorkDir(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "module")
	workDir := filepath.Join(target, "testdata")
	if err := os.MkdirAll(workDir, 0o750); err != nil {
		t.Fatal(err)
	}

	actual, err := resolveWorkDir(root, "module", "testdata")
	if err != nil {
		t.Fatal(err)
	}
	if actual != workDir {
		t.Fatalf("expected %s, got %s", workDir, actual)
	}
	if _, err := resolveWorkDir(root, "module", "../../outside"); err == nil {
		t.Fatal("expected escaping working directory to fail")
	}
}

func TestNewBenchmarkCommandAppliesLimits(t *testing.T) {
	r := runner{
		goRoot: "/toolchain",
		opts: options{
			GoMaxProcs: 2,
			MaxRSSMiB:  2048,
			Nice:       10,
		},
	}
	cmd := r.newBenchmarkCommand(
		binary{Path: "/bin/linter"}, "/work", "/cache", []string{"run"}, nil, io.Discard,
	)
	for _, expected := range []string{"GOMAXPROCS=2", "GOMEMLIMIT=2048MiB", "GOFLAGS=-p=2"} {
		if !slices.Contains(cmd.Env, expected) {
			t.Fatalf("expected %q in environment", expected)
		}
	}
	if runtime.GOOS != "windows" && cmd.Path != "/usr/bin/nice" {
		t.Fatalf("expected nice wrapper, got %q", cmd.Path)
	}
}

func TestDirectorySize(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "one"), []byte("123"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "nested"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nested", "two"), []byte("4567"), 0o600); err != nil {
		t.Fatal(err)
	}

	actual, err := directorySize(dir)
	if err != nil {
		t.Fatal(err)
	}
	if actual != 7 {
		t.Fatalf("expected 7 bytes, got %d", actual)
	}
}

func TestCorruptCacheData(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "00", "entry-d")
	index := filepath.Join(dir, "00", "entry-a")
	require.NoError(t, os.MkdirAll(filepath.Dir(data), 0o750))
	require.NoError(t, os.WriteFile(data, []byte("valid data"), 0o600))
	require.NoError(t, os.WriteFile(index, []byte("valid index"), 0o600))

	count, err := corruptCacheData(dir)
	require.NoError(t, err)
	require.Equal(t, 1, count)
	actual, err := os.ReadFile(data)
	require.NoError(t, err)
	require.Equal(t, "corrupted cache data\n", string(actual))
	actual, err = os.ReadFile(index)
	require.NoError(t, err)
	require.Equal(t, "valid index", string(actual))
}

func TestExpectedFilesMatch(t *testing.T) {
	actualRoot := t.TempDir()
	goldenRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(actualRoot, "actual.go"), []byte("same"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(goldenRoot, "expected.go"), []byte("same"), 0o600))
	expectedFiles, err := loadExpectedFiles(goldenRoot, map[string]string{"actual.go": "expected.go"})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(goldenRoot, "expected.go"), []byte("dirty"), 0o600))

	match, err := expectedFilesMatch(actualRoot, expectedFiles)
	require.NoError(t, err)
	require.True(t, match)
	require.NoError(t, os.WriteFile(filepath.Join(actualRoot, "actual.go"), []byte("different"), 0o600))
	match, err = expectedFilesMatch(actualRoot, expectedFiles)
	require.NoError(t, err)
	require.False(t, match)
	require.NoError(t, os.Remove(filepath.Join(actualRoot, "actual.go")))
	match, err = expectedFilesMatch(actualRoot, expectedFiles)
	require.NoError(t, err)
	require.False(t, match)
}

func TestCompatibilityWorktreesAreIsolatedAndRemoved(t *testing.T) {
	repository := filepath.Join(t.TempDir(), "repository")
	require.NoError(t, os.Mkdir(repository, 0o750))
	runGitTest(t, repository, "init", "--quiet")
	require.NoError(t, os.WriteFile(filepath.Join(repository, "source.go"), []byte("before"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repository, "config.yml"), []byte("version: '2'\n"), 0o600))
	runGitTest(t, repository, "add", "source.go", "config.yml")
	runGitTest(t, repository,
		"-c", "user.name=Benchmark Test",
		"-c", "user.email=benchmark@example.com",
		"-c", "commit.gpgsign=false",
		"commit", "--quiet", "-m", "initial",
	)
	revision := strings.TrimSpace(runGitTest(t, repository, "rev-parse", "HEAD"))

	runnerCtx, cancelRunner := context.WithCancel(t.Context())
	r := runner{ctx: runnerCtx, outDir: filepath.Join(t.TempDir(), "artifacts")}
	workload := &preparedWorkload{
		workload: workload{Name: "fixture", Revision: revision, Config: "config.yml"},
		Root:     repository,
		Targets:  []string{"."},
	}
	reference, err := r.createCompatibilityWorktree(workload, "case", upstreamLabel)
	require.NoError(t, err)
	candidate, err := r.createCompatibilityWorktree(workload, "case", forkLabel)
	require.NoError(t, err)
	require.NotEqual(t, reference.Root, candidate.Root)
	require.True(t, strings.HasPrefix(reference.ConfigPath, reference.Root))
	require.True(t, strings.HasPrefix(candidate.ConfigPath, candidate.Root))
	require.NoError(t, os.WriteFile(filepath.Join(reference.Root, "source.go"), []byte("fixed"), 0o600))
	candidateData, err := os.ReadFile(filepath.Join(candidate.Root, "source.go"))
	require.NoError(t, err)
	require.Equal(t, "before", string(candidateData))

	cancelRunner()
	require.NoError(t, r.removeCompatibilityWorktree(repository, candidate.Root))
	require.NoError(t, r.removeCompatibilityWorktree(repository, reference.Root))
	listed := runGitTest(t, repository, "worktree", "list", "--porcelain")
	require.NotContains(t, listed, reference.Root)
	require.NotContains(t, listed, candidate.Root)
}

func runGitTest(t *testing.T, repository string, args ...string) string {
	t.Helper()
	commandArgs := append([]string{"-C", repository}, args...)
	output, err := exec.CommandContext(t.Context(), "git", commandArgs...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}

	return string(output)
}
