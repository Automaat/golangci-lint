package diagnostics

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/golangci/golangci-lint/v2/scripts/bench/internal/changes"
)

func TestNormalizeReportData(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "work", "repo")
	input := `{
  "Issues": [
    {"FromLinter":"z","Text":"second","Pos":{"Filename":` + strconv.Quote(filepath.Join(root, "z.go")) + `}},
    {"FromLinter":"a","Text":"first","Pos":{"Filename":` + strconv.Quote(filepath.Join(root, "a.go")) + `},
     "SuggestedFixes":[{"Message":"z"},{"Message":"a"}]}
  ],
  "Report": {"Linters":[{"Name":"z"},{"Name":"a"}],"Warnings":null}
}`

	report, err := normalizeReportData([]byte(input), root)
	if err != nil {
		t.Fatal(err)
	}
	if report.issueCount != 2 {
		t.Fatalf("expected 2 issues, got %d", report.issueCount)
	}
	if bytes.Contains(report.data, []byte(root)) {
		t.Fatalf("workload root was not normalized: %s", report.data)
	}
	if !bytes.Contains(report.data, []byte(`"Filename": "$WORKLOAD`)) {
		t.Fatalf("normalized workload marker is missing: %s", report.data)
	}
	if bytes.Index(report.data, []byte(`"FromLinter": "a"`)) > bytes.Index(report.data, []byte(`"FromLinter": "z"`)) {
		t.Fatalf("issues were not sorted: %s", report.data)
	}
	if bytes.Index(report.data, []byte(`"Name": "a"`)) > bytes.Index(report.data, []byte(`"Name": "z"`)) {
		t.Fatalf("linters were not sorted: %s", report.data)
	}
	if bytes.Index(report.data, []byte(`"Message": "a"`)) > bytes.Index(report.data, []byte(`"Message": "z"`)) {
		t.Fatalf("suggested fixes were not sorted: %s", report.data)
	}
}

func TestNormalizeReportDataRejectsTrailingJSON(t *testing.T) {
	if _, err := normalizeReportData([]byte(`{} {}`), "/work"); err == nil {
		t.Fatal("expected trailing JSON to fail")
	}
}

func TestNormalizeReportDataWindowsPath(t *testing.T) {
	root := `C:\work\repo`
	filename := root + `\file.go`
	input := `{"Issues":[{"Pos":{"Filename":` + strconv.Quote(filename) + `}}]}`

	report, err := normalizeReportData([]byte(input), root)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(report.data, []byte(root)) {
		t.Fatalf("workload root was not normalized: %s", report.data)
	}
}

func TestCompareFilesUsesSeparateRootsAndChanges(t *testing.T) {
	referenceRoot := filepath.Join(t.TempDir(), "reference")
	candidateRoot := filepath.Join(t.TempDir(), "candidate")
	if err := os.Mkdir(referenceRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(candidateRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	referenceReport := filepath.Join(t.TempDir(), "reference.json")
	candidateReport := filepath.Join(t.TempDir(), "candidate.json")
	writeTestReport(t, referenceReport, filepath.Join(referenceRoot, "fixed.go"))
	writeTestReport(t, candidateReport, filepath.Join(candidateRoot, "fixed.go"))

	state := changes.State{Type: "regular", Mode: 0o600, SHA256: "digest"}
	changeSet := changes.Set{
		SchemaVersion: changes.SchemaVersion,
		Changes:       []changes.Change{{Path: "fixed.go", Kind: "modify", After: &state}},
	}
	expected := true
	result, err := CompareFiles(
		Input{Path: referenceReport, Root: referenceRoot, ExitCode: 1, Changes: &changeSet, ExpectedMatch: &expected},
		Input{Path: candidateReport, Root: candidateRoot, ExitCode: 1, Changes: &changeSet, ExpectedMatch: &expected},
		filepath.Join(t.TempDir(), "comparison"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Match || !result.DiagnosticsMatch || !result.ChangesMatch || !result.ReferenceExpectedMatch {
		t.Fatalf("unexpected summary: %+v", result)
	}
	if result.ReferenceChangesSHA256 == "" || result.ReferenceChangesSHA256 != result.CandidateChangesSHA256 {
		t.Fatalf("unexpected change hashes: %+v", result)
	}
}

func TestCompareFilesRecordsRetainedWorktreesOnMismatch(t *testing.T) {
	root := t.TempDir()
	referenceReport := filepath.Join(root, "reference.json")
	candidateReport := filepath.Join(root, "candidate.json")
	writeTestReport(t, referenceReport, filepath.Join(root, "fixed.go"))
	writeTestReport(t, candidateReport, filepath.Join(root, "fixed.go"))

	referenceState := changes.State{Type: "regular", Mode: 0o600, SHA256: "reference"}
	candidateState := changes.State{Type: "regular", Mode: 0o600, SHA256: "candidate"}
	referenceChanges := changes.Set{
		SchemaVersion: changes.SchemaVersion,
		Changes:       []changes.Change{{Path: "fixed.go", Kind: "modify", After: &referenceState}},
	}
	candidateChanges := changes.Set{
		SchemaVersion: changes.SchemaVersion,
		Changes:       []changes.Change{{Path: "fixed.go", Kind: "modify", After: &candidateState}},
	}
	result, err := CompareFiles(
		Input{
			Path: referenceReport, Root: root, Changes: &referenceChanges, WorktreePath: "/retained/reference",
		},
		Input{
			Path: candidateReport, Root: root, Changes: &candidateChanges, WorktreePath: "/retained/candidate",
		},
		filepath.Join(root, "comparison"),
	)
	if err == nil {
		t.Fatal("expected mismatch")
	}
	if result.Match || result.ChangesMatch {
		t.Fatalf("unexpected match: %+v", result)
	}
	if result.ReferenceWorktree != "/retained/reference" || result.CandidateWorktree != "/retained/candidate" {
		t.Fatalf("retained worktrees missing: %+v", result)
	}
}

func writeTestReport(t *testing.T, path, filename string) {
	t.Helper()
	data := []byte(`{"Issues":[{"Pos":{"Filename":` + strconv.Quote(filename) + `}}]}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
