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
)

func TestStartCommandStopsAtTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires /bin/sleep")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sleep", "30")
	finished, _, err := startCommand(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("expected timed out command to fail")
	}
	close(finished)
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
