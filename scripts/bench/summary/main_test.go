package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeResults(t *testing.T, root, order string, records ...record) {
	t.Helper()
	dir := filepath.Join(root, order)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, r := range records {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(b))
	}
	if err := os.WriteFile(filepath.Join(dir, "results.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func timing(binary, mode string, wallSeconds float64, exit int) record {
	return record{
		Binary: binary, CacheMode: mode, Purpose: "timing", ExitCode: exit,
		WallNS: int64(wallSeconds * 1e9), UserCPUNS: int64(2 * wallSeconds * 1e9), PeakRSS: 512 << 20,
	}
}

func TestSummaryMediansAndOrders(t *testing.T) {
	root := t.TempDir()
	writeResults(t, root, "candidate-first",
		timing("fork", "cold", 8, 0), timing("fork", "cold", 9, 0),
		timing("upstream", "cold", 10, 0), timing("upstream", "cold", 10, 0),
		record{Binary: "fork", CacheMode: "cold", Purpose: "prepare", WallNS: 99e9},
	)
	writeResults(t, root, "baseline-first",
		timing("fork", "cold", 9, 0), timing("upstream", "cold", 10, 0),
		timing("fork", "cold", 1, 1),
		timing("fork", "warm", 1, 0), timing("upstream", "warm", 2, 0),
	)

	samples, err := load(root)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := write(&out, samples); err != nil {
		t.Fatal(err)
	}
	got := out.String()

	for _, want := range []string{
		"1 timing samples exited non-zero",
		"### cold cache (3 candidate / 3 baseline samples)",
		"| wall | 10.000 s | 9.000 s | -10.0% |",
		"| user CPU | 20.000 s | 18.000 s | -10.0% |",
		"| peak tree RSS | 512.000 MiB | 512.000 MiB | +0.0% |",
		"| wall, baseline-first | 10.000 s | 9.000 s | -10.0% |",
		"| wall, candidate-first | 10.000 s | 8.500 s | -15.0% |",
		"### warm cache (1 candidate / 1 baseline samples)",
		"| wall | 2.000 s | 1.000 s | -50.0% |",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary missing %q:\n%s", want, got)
		}
	}
}

func TestSummaryWithoutTimingSamples(t *testing.T) {
	root := t.TempDir()
	writeResults(t, root, "candidate-first", record{Binary: "fork", Purpose: "prepare"})
	if _, err := load(root); err == nil {
		t.Fatal("expected an error without timing samples")
	}
}
