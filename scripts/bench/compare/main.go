package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/golangci/golangci-lint/v2/scripts/bench/internal/diagnostics"
)

type options struct {
	ReferencePath string
	CandidatePath string
	WorkloadRoot  string
	OutputDir     string
	ReferenceExit int
	CandidateExit int
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "diagnostic comparison: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	opts, err := parseOptions(args)
	if err != nil {
		return err
	}

	result, err := diagnostics.CompareFiles(
		diagnostics.Input{Path: opts.ReferencePath, ExitCode: opts.ReferenceExit},
		diagnostics.Input{Path: opts.CandidatePath, ExitCode: opts.CandidateExit},
		opts.WorkloadRoot,
		opts.OutputDir,
	)
	if err != nil {
		return err
	}

	_, _ = fmt.Fprintf(os.Stdout, "diagnostics match: %d issues, exit code %d\n", result.ReferenceIssues, result.ReferenceExit)

	return nil
}

func parseOptions(args []string) (options, error) {
	var opts options

	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&opts.ReferencePath, "reference", "", "reference JSON output")
	fs.StringVar(&opts.CandidatePath, "candidate", "", "candidate JSON output")
	fs.StringVar(&opts.WorkloadRoot, "workload-root", "", "absolute workload root to normalize")
	fs.StringVar(&opts.OutputDir, "out", "", "new output directory")
	fs.IntVar(&opts.ReferenceExit, "reference-exit", 0, "reference process exit code")
	fs.IntVar(&opts.CandidateExit, "candidate-exit", 0, "candidate process exit code")
	if err := fs.Parse(args); err != nil {
		return options{}, fmt.Errorf("parse flags: %w", err)
	}
	if fs.NArg() != 0 {
		return options{}, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	setFlags := map[string]bool{}
	fs.Visit(func(item *flag.Flag) {
		setFlags[item.Name] = true
	})
	required := []struct {
		name  string
		value string
	}{
		{name: "reference", value: opts.ReferencePath},
		{name: "candidate", value: opts.CandidatePath},
		{name: "workload-root", value: opts.WorkloadRoot},
		{name: "out", value: opts.OutputDir},
	}
	for _, item := range required {
		if item.value == "" {
			return options{}, fmt.Errorf("--%s is required", item.name)
		}
	}
	for _, name := range []string{"reference-exit", "candidate-exit"} {
		if !setFlags[name] {
			return options{}, fmt.Errorf("--%s is required", name)
		}
	}
	if !filepath.IsAbs(opts.WorkloadRoot) {
		return options{}, errors.New("--workload-root must be absolute")
	}

	return opts, nil
}
