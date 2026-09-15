package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

const (
	schemaVersion   = 1
	privateDirMode  = 0o750
	privateFileMode = 0o600
)

type options struct {
	ReferencePath string
	CandidatePath string
	WorkloadRoot  string
	OutputDir     string
	ReferenceExit int
	CandidateExit int
}

type normalizedReport struct {
	Data       []byte
	IssueCount int
}

type summary struct {
	SchemaVersion    int    `json:"schema_version"`
	Match            bool   `json:"match"`
	DiagnosticsMatch bool   `json:"diagnostics_match"`
	ExitCodesMatch   bool   `json:"exit_codes_match"`
	ReferenceExit    int    `json:"reference_exit_code"`
	CandidateExit    int    `json:"candidate_exit_code"`
	ReferenceIssues  int    `json:"reference_issue_count"`
	CandidateIssues  int    `json:"candidate_issue_count"`
	ReferenceSHA256  string `json:"reference_sha256"`
	CandidateSHA256  string `json:"candidate_sha256"`
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

	referenceData, err := os.ReadFile(opts.ReferencePath)
	if err != nil {
		return fmt.Errorf("read reference output: %w", err)
	}
	candidateData, err := os.ReadFile(opts.CandidatePath)
	if err != nil {
		return fmt.Errorf("read candidate output: %w", err)
	}

	reference, err := normalizeReportData(referenceData, opts.WorkloadRoot)
	if err != nil {
		return fmt.Errorf("normalize reference output: %w", err)
	}
	candidate, err := normalizeReportData(candidateData, opts.WorkloadRoot)
	if err != nil {
		return fmt.Errorf("normalize candidate output: %w", err)
	}

	if err := prepareOutputDir(opts.OutputDir); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(opts.OutputDir, "reference.normalized.json"), reference.Data); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(opts.OutputDir, "candidate.normalized.json"), candidate.Data); err != nil {
		return err
	}

	diagnosticsMatch := bytes.Equal(reference.Data, candidate.Data)
	exitCodesMatch := opts.ReferenceExit == opts.CandidateExit
	result := summary{
		SchemaVersion:    schemaVersion,
		Match:            diagnosticsMatch && exitCodesMatch,
		DiagnosticsMatch: diagnosticsMatch,
		ExitCodesMatch:   exitCodesMatch,
		ReferenceExit:    opts.ReferenceExit,
		CandidateExit:    opts.CandidateExit,
		ReferenceIssues:  reference.IssueCount,
		CandidateIssues:  candidate.IssueCount,
		ReferenceSHA256:  sha256Hex(reference.Data),
		CandidateSHA256:  sha256Hex(candidate.Data),
	}
	if err := writeJSON(filepath.Join(opts.OutputDir, "summary.json"), result); err != nil {
		return err
	}
	if !result.Match {
		return fmt.Errorf("compatibility mismatch; see %s", opts.OutputDir)
	}

	_, _ = fmt.Fprintf(os.Stdout, "diagnostics match: %d issues, exit code %d\n", reference.IssueCount, opts.ReferenceExit)

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

func normalizeReportData(data []byte, workloadRoot string) (normalizedReport, error) {
	var value any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&value); err != nil {
		return normalizedReport{}, err
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return normalizedReport{}, errors.New("unexpected trailing JSON value")
		}
		return normalizedReport{}, fmt.Errorf("read trailing data: %w", err)
	}

	root, ok := value.(map[string]any)
	if !ok {
		return normalizedReport{}, errors.New("output must be a JSON object")
	}
	normalizeValue(root, rootVariants(workloadRoot))
	issues, err := sortObjectArray(root, "Issues")
	if err != nil {
		return normalizedReport{}, err
	}
	if report, ok := root["Report"].(map[string]any); ok {
		if _, sortErr := sortObjectArray(report, "Linters"); sortErr != nil {
			return normalizedReport{}, sortErr
		}
		if _, sortErr := sortObjectArray(report, "Warnings"); sortErr != nil {
			return normalizedReport{}, sortErr
		}
	}

	normalized, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return normalizedReport{}, err
	}
	normalized = append(normalized, '\n')

	return normalizedReport{Data: normalized, IssueCount: issues}, nil
}

func normalizeValue(value any, replacements []string) any {
	switch typed := value.(type) {
	case string:
		for _, replacement := range replacements {
			typed = strings.ReplaceAll(typed, replacement, "$WORKLOAD")
		}
		return typed
	case map[string]any:
		for key, item := range typed {
			typed[key] = normalizeValue(item, replacements)
		}
	case []any:
		for index, item := range typed {
			typed[index] = normalizeValue(item, replacements)
		}
	}

	return value
}

func rootVariants(root string) []string {
	clean := filepath.Clean(root)
	variants := []string{clean, filepath.ToSlash(clean)}
	slices.Sort(variants)
	return slices.Compact(variants)
}

func sortObjectArray(parent map[string]any, key string) (int, error) {
	value, exists := parent[key]
	if !exists || value == nil {
		parent[key] = []any{}
		return 0, nil
	}
	items, ok := value.([]any)
	if !ok {
		return 0, fmt.Errorf("%s must be an array", key)
	}
	type sortableItem struct {
		value any
		key   []byte
	}
	sorted := make([]sortableItem, 0, len(items))
	for _, item := range items {
		encoded, err := json.Marshal(item)
		if err != nil {
			return 0, fmt.Errorf("encode %s item: %w", key, err)
		}
		sorted = append(sorted, sortableItem{value: item, key: encoded})
	}
	sort.Slice(sorted, func(i, j int) bool {
		return bytes.Compare(sorted[i].key, sorted[j].key) < 0
	})
	for index, item := range sorted {
		items[index] = item.value
	}

	return len(items), nil
}

func prepareOutputDir(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve output directory: %w", err)
	}
	if _, err := os.Stat(abs); err == nil {
		return fmt.Errorf("output directory already exists: %s", abs)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat output directory: %w", err)
	}
	if err := os.MkdirAll(abs, privateDirMode); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}

	return nil
}

func writeFile(path string, data []byte) error {
	if err := os.WriteFile(path, data, privateFileMode); err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}

	return nil
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(path, append(data, '\n'))
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
