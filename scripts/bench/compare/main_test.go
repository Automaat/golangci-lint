package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeReportData(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "work", "repo")
	input := `{
  "Issues": [
    {"FromLinter":"z","Text":"second","Pos":{"Filename":"` + filepath.Join(root, "z.go") + `"}},
    {"FromLinter":"a","Text":"first","Pos":{"Filename":"` + filepath.Join(root, "a.go") + `"}}
  ],
  "Report": {"Linters":[{"Name":"z"},{"Name":"a"}],"Warnings":null}
}`

	report, err := normalizeReportData([]byte(input), root)
	if err != nil {
		t.Fatal(err)
	}
	if report.IssueCount != 2 {
		t.Fatalf("expected 2 issues, got %d", report.IssueCount)
	}
	if bytes.Contains(report.Data, []byte(root)) {
		t.Fatalf("workload root was not normalized: %s", report.Data)
	}
	if !bytes.Contains(report.Data, []byte(`"Filename": "$WORKLOAD`)) {
		t.Fatalf("normalized workload marker is missing: %s", report.Data)
	}
	if bytes.Index(report.Data, []byte(`"FromLinter": "a"`)) > bytes.Index(report.Data, []byte(`"FromLinter": "z"`)) {
		t.Fatalf("issues were not sorted: %s", report.Data)
	}
	if bytes.Index(report.Data, []byte(`"Name": "a"`)) > bytes.Index(report.Data, []byte(`"Name": "z"`)) {
		t.Fatalf("linters were not sorted: %s", report.Data)
	}
}

func TestNormalizeReportDataRejectsTrailingJSON(t *testing.T) {
	if _, err := normalizeReportData([]byte(`{} {}`), "/work"); err == nil {
		t.Fatal("expected trailing JSON to fail")
	}
}

func TestParseOptionsRequiresExitCodes(t *testing.T) {
	_, err := parseOptions([]string{
		"--reference", "reference.json",
		"--candidate", "candidate.json",
		"--workload-root", "/work",
		"--out", "comparison",
	})
	if err == nil {
		t.Fatal("expected missing exit codes to fail")
	}
}

func TestRunDetectsExitCodeMismatch(t *testing.T) {
	dir := t.TempDir()
	referencePath := filepath.Join(dir, "reference.json")
	candidatePath := filepath.Join(dir, "candidate.json")
	for _, path := range []string{referencePath, candidatePath} {
		if err := os.WriteFile(path, []byte(`{"Issues":[],"Report":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	outDir := filepath.Join(dir, "comparison")
	err := run([]string{
		"--reference", referencePath,
		"--candidate", candidatePath,
		"--workload-root", dir,
		"--reference-exit", "1",
		"--candidate-exit", "2",
		"--out", outDir,
	})
	if err == nil {
		t.Fatal("expected exit code mismatch")
	}
	if _, statErr := os.Stat(filepath.Join(outDir, "summary.json")); statErr != nil {
		t.Fatal(statErr)
	}
}
