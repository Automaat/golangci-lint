package main

import (
	"os"
	"path/filepath"
	"testing"
)

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
