package test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/golangci/golangci-lint/v2/test/testshared"
)

var narrowingFixture = map[string]string{
	"go.mod": "module example.com/m\n\ngo 1.22\n",
	"a/a.go": `package a

import "os"

func Remove(path string) {
	os.Remove(path)
}
`,
	"b/b.go": `package b

import (
	"os"

	"example.com/m/a"
)

func Clean(path string) {
	a.Remove(path)
	os.Chdir(path)
}
`,
	"c/c.go": `package c

import "os"

func Touch(path string) {
	os.Mkdir(path, 0o750)
}
`,
}

type narrowingRun struct {
	issues []string
	log    string
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()

	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
}

func commitAll(t *testing.T, dir string) {
	t.Helper()

	for _, cmd := range []*exec.Cmd{
		exec.CommandContext(t.Context(), "git", "init", "-q"),
		exec.CommandContext(t.Context(), "git", "add", "."),
		exec.CommandContext(t.Context(), "git", "commit", "-q", "-m", "initial"),
	} {
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		)

		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
	}
}

func runNarrowing(t *testing.T, binPath, dir string, analyzeAll bool) narrowingRun {
	t.Helper()

	jsonPath := filepath.Join(t.TempDir(), "issues.json")

	environ := []string{"GOFLAGS="}
	if analyzeAll {
		environ = append(environ, "GOLANGCI_LINT_DIFF_ANALYZE_ALL=1")
	}

	cmd := testshared.NewRunnerBuilder(t).
		WithBinPath(binPath).
		WithNoConfig().
		WithEnviron(environ...).
		WithArgs("-v", "--default=none", "-Eerrcheck,govet,staticcheck,unused", "--new-from-rev=HEAD",
			"--output.json.path="+jsonPath, "--output.text.path=stderr", "./...").
		Runner().
		Command()
	cmd.Dir = dir

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	err := cmd.Run()
	var exitErr *exec.ExitError
	if err != nil && (!errors.As(err, &exitErr) || exitErr.ExitCode() != 1) {
		t.Fatalf("golangci-lint failed: %v\n%s", err, stderr.String())
	}

	content, err := os.ReadFile(jsonPath)
	require.NoError(t, err, stderr.String())

	var report struct {
		Issues []struct {
			FromLinter string
			Text       string
			Pos        struct {
				Filename     string
				Line, Column int
			}
		}
	}
	require.NoError(t, json.Unmarshal(content, &report))

	var issues []string
	for _, issue := range report.Issues {
		issues = append(issues, strings.Join([]string{
			issue.FromLinter, filepath.ToSlash(issue.Pos.Filename), issue.Text,
		}, " "))
	}
	sort.Strings(issues)

	return narrowingRun{issues: issues, log: stderr.String()}
}

func TestDiffNarrowingMatchesFullAnalysis(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("diff modes depend on git line endings and path handling that are not exercised on Windows")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}

	binPath := testshared.InstallGolangciLint(t)

	testCases := []struct {
		desc      string
		change    map[string]string
		wantLog   string
		wantTexts []string
	}{
		{
			desc: "edit in one package",
			change: map[string]string{"c/c.go": `package c

import "os"

func Touch(path string) {
	os.Mkdir(path, 0o750)
	os.Remove(path)
}
`},
			wantLog:   "Analyzing 1 of 3 packages",
			wantTexts: []string{"os.Remove"},
		},
		{
			desc: "API change breaks an unchanged importer",
			change: map[string]string{"a/a.go": `package a

import "os"

func Delete(path string) error {
	return os.Remove(path)
}
`},
			wantLog:   "Analyzing 2 of 3 packages",
			wantTexts: []string{"undefined: a.Remove"},
		},
		{
			desc:    "module file change",
			change:  map[string]string{"go.mod": "module example.com/m\n\ngo 1.22\n\n// comment\n"},
			wantLog: "Analyzing all packages",
		},
	}

	for _, test := range testCases {
		t.Run(test.desc, func(t *testing.T) {
			dir := t.TempDir()
			writeFiles(t, dir, narrowingFixture)
			commitAll(t, dir)
			writeFiles(t, dir, test.change)

			narrowed := runNarrowing(t, binPath, dir, false)
			full := runNarrowing(t, binPath, dir, true)

			assert.Equal(t, full.issues, narrowed.issues)
			assert.Contains(t, narrowed.log, test.wantLog)
			assert.NotContains(t, full.log, "Analyzing 1 of")

			for _, text := range test.wantTexts {
				assert.Contains(t, strings.Join(narrowed.issues, "\n"), text)
			}
		})
	}
}
