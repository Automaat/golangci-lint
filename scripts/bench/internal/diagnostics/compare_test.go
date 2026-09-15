package diagnostics

import (
	"bytes"
	"path/filepath"
	"strconv"
	"testing"
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
