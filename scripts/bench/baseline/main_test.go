package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

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
		scenario{Name: "configured"},
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

func TestReplaceEnv(t *testing.T) {
	actual := replaceEnv([]string{"PATH=old", "KEEP=value", "GOROOT=old"}, "PATH=new", "GOROOT=new")
	expected := []string{"KEEP=value", "PATH=new", "GOROOT=new"}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("expected %v, got %v", expected, actual)
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
