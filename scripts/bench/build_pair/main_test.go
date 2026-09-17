package main

import (
	"reflect"
	"testing"
	"time"
)

func TestParseOptionsDefaults(t *testing.T) {
	opts, err := parseOptions(nil)
	if err != nil {
		t.Fatal(err)
	}
	if opts.CandidateRef != "HEAD" || opts.UpstreamRef != "upstream/main" ||
		opts.BaselineMode != baselineModeMergeBase || opts.OutputDir != "dist/bench/bin" {
		t.Fatalf("unexpected refs or output: %+v", opts)
	}
	if opts.BuildTimeout != defaultBuildTimeout || opts.MaxMemoryMiB != defaultMaxMemoryMiB ||
		opts.GoMaxProcs != defaultGoMaxProcs || opts.Nice != defaultNice {
		t.Fatalf("unexpected safety defaults: %+v", opts)
	}
}

func TestParseOptionsRejectsUnsafeLimits(t *testing.T) {
	for _, args := range [][]string{
		{"--build-timeout", "0s"},
		{"--max-memory-mib", "0"},
		{"--go-max-procs", "0"},
		{"--nice", "21"},
		{"--baseline-mode", "unknown"},
	} {
		if _, err := parseOptions(args); err == nil {
			t.Fatalf("expected %v to fail", args)
		}
	}
}

func TestParseOptionsOverrides(t *testing.T) {
	opts, err := parseOptions([]string{
		"--candidate-ref", "feature",
		"--upstream-ref", "origin/main",
		"--baseline-mode", "exact",
		"--out-dir", "artifacts",
		"--build-timeout", "1m",
		"--max-memory-mib", "512",
		"--go-max-procs", "1",
		"--nice", "5",
	})
	if err != nil {
		t.Fatal(err)
	}
	if opts.CandidateRef != "feature" || opts.UpstreamRef != "origin/main" ||
		opts.BaselineMode != baselineModeExact || opts.OutputDir != "artifacts" ||
		opts.BuildTimeout != time.Minute || opts.MaxMemoryMiB != 512 || opts.GoMaxProcs != 1 || opts.Nice != 5 {
		t.Fatalf("unexpected options: %+v", opts)
	}
}

func TestSelectBaselineSHA(t *testing.T) {
	if actual := selectBaselineSHA(baselineModeMergeBase, "merge", "upstream"); actual != "merge" {
		t.Fatalf("expected merge base, got %q", actual)
	}
	if actual := selectBaselineSHA(baselineModeExact, "merge", "upstream"); actual != "upstream" {
		t.Fatalf("expected exact upstream, got %q", actual)
	}
}

func TestReplaceEnv(t *testing.T) {
	actual := replaceEnv([]string{"PATH=old", "KEEP=value", "GOFLAGS=old"}, "PATH=new", "GOFLAGS=-p=2")
	expected := []string{"KEEP=value", "PATH=new", "GOFLAGS=-p=2"}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("expected %v, got %v", expected, actual)
	}
}
