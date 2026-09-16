package lifecycle

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	SchemaVersion = 1
	EnvReportPath = "GOLANGCI_LIFECYCLE_REPORT"
)

// PackageLoad describes the package-loading phase.
type PackageLoad struct {
	ElapsedNS        int64  `json:"elapsed_ns"`
	OriginalPackages int    `json:"original_packages"`
	DeduplicatedPkgs int    `json:"deduplicated_packages"`
	Error            string `json:"error,omitempty"`
}

// LinterRun describes one top-level linter invocation.
type LinterRun struct {
	Name      string `json:"name"`
	ElapsedNS int64  `json:"elapsed_ns"`
	Issues    int    `json:"issues"`
	Error     string `json:"error,omitempty"`
}

// ConfiguredLinterRun describes one configured Go-analysis linter.
type ConfiguredLinterRun struct {
	Name      string `json:"name"`
	ElapsedNS int64  `json:"elapsed_ns"`
	Actions   int    `json:"actions"`
	Issues    int    `json:"issues"`
	Errors    int    `json:"errors"`
}

// AnalyzerRun describes one analyzer across its package actions.
type AnalyzerRun struct {
	Name            string `json:"name"`
	Linter          string `json:"linter,omitempty"`
	ElapsedNS       int64  `json:"elapsed_ns"`
	Actions         int    `json:"actions"`
	ExecutedActions int    `json:"executed_actions"`
	Diagnostics     int    `json:"diagnostics"`
	Errors          int    `json:"errors"`
}

// AnalysisRun describes one Go analysis graph execution.
type AnalysisRun struct {
	Name              string                `json:"name"`
	ElapsedNS         int64                 `json:"elapsed_ns"`
	RequestedPkgs     int                   `json:"requested_packages"`
	CachedPkgs        int                   `json:"cached_packages"`
	AnalyzedPkgs      int                   `json:"analyzed_packages"`
	InitialPkgs       int                   `json:"initial_packages"`
	TotalPkgs         int                   `json:"total_packages"`
	Actions           int                   `json:"actions"`
	Parallelism       int                   `json:"parallelism"`
	ConfiguredLinters []ConfiguredLinterRun `json:"configured_linters"`
	Analyzers         []AnalyzerRun         `json:"analyzers"`
	Error             string                `json:"error,omitempty"`
}

// Processing describes the result-processing pipeline.
type Processing struct {
	ElapsedNS int64 `json:"elapsed_ns"`
	Input     int   `json:"input_issues"`
	Output    int   `json:"output_issues"`
}

// Outcome describes the CLI-visible result.
type Outcome struct {
	ExitCode     int    `json:"exit_code"`
	Error        string `json:"error,omitempty"`
	ContextError string `json:"context_error,omitempty"`
}

// Report is the versioned lifecycle report.
type Report struct {
	SchemaVersion int           `json:"schema_version"`
	StartedAt     time.Time     `json:"started_at"`
	ElapsedNS     int64         `json:"elapsed_ns"`
	PackageLoad   *PackageLoad  `json:"package_load,omitempty"`
	Linters       []LinterRun   `json:"linters"`
	Analysis      []AnalysisRun `json:"analysis"`
	Processing    *Processing   `json:"processing,omitempty"`
	Outcome       *Outcome      `json:"outcome,omitempty"`
}

// Recorder collects lifecycle events when explicitly enabled.
type Recorder struct {
	mu      sync.Mutex
	started time.Time
	report  Report
}

// NewRecorder starts a schema-versioned lifecycle recording.
func NewRecorder() *Recorder {
	started := time.Now()

	return &Recorder{
		started: started,
		report: Report{
			SchemaVersion: SchemaVersion,
			StartedAt:     started.UTC(),
			Linters:       []LinterRun{},
			Analysis:      []AnalysisRun{},
		},
	}
}

// RecordPackageLoad records the single package-loading phase.
func (r *Recorder) RecordPackageLoad(elapsed time.Duration, original, deduplicated int, err error) {
	if r == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.report.PackageLoad = &PackageLoad{
		ElapsedNS:        elapsed.Nanoseconds(),
		OriginalPackages: original,
		DeduplicatedPkgs: deduplicated,
		Error:            errorString(err),
	}
}

// RecordLinter records one sequential top-level linter invocation.
func (r *Recorder) RecordLinter(name string, elapsed time.Duration, issues int, err error) {
	if r == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.report.Linters = append(r.report.Linters, LinterRun{
		Name:      name,
		ElapsedNS: elapsed.Nanoseconds(),
		Issues:    issues,
		Error:     errorString(err),
	})
}

// RecordAnalysis records one Go analysis graph execution.
func (r *Recorder) RecordAnalysis(run *AnalysisRun) {
	if r == nil || run == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	value := *run
	value.ConfiguredLinters = cloneSlice(run.ConfiguredLinters)
	value.Analyzers = cloneSlice(run.Analyzers)
	r.report.Analysis = append(r.report.Analysis, value)
}

// RecordProcessing records the complete result-processing pipeline.
func (r *Recorder) RecordProcessing(elapsed time.Duration, input, output int) {
	if r == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.report.Processing = &Processing{
		ElapsedNS: elapsed.Nanoseconds(),
		Input:     input,
		Output:    output,
	}
}

// Finish records the CLI-visible outcome.
func (r *Recorder) Finish(exitCode int, runErr, contextErr error) {
	if r == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.report.ElapsedNS = time.Since(r.started).Nanoseconds()
	r.report.Outcome = &Outcome{
		ExitCode:     exitCode,
		Error:        errorString(runErr),
		ContextError: errorString(contextErr),
	}
}

// Snapshot returns an isolated copy of the current report.
func (r *Recorder) Snapshot() Report {
	if r == nil {
		return Report{}
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	report := r.report
	report.Linters = cloneSlice(r.report.Linters)
	report.Analysis = cloneSlice(r.report.Analysis)
	for i := range report.Analysis {
		report.Analysis[i].ConfiguredLinters = cloneSlice(report.Analysis[i].ConfiguredLinters)
		report.Analysis[i].Analyzers = cloneSlice(report.Analysis[i].Analyzers)
	}
	if r.report.PackageLoad != nil {
		value := *r.report.PackageLoad
		report.PackageLoad = &value
	}
	if r.report.Processing != nil {
		value := *r.report.Processing
		report.Processing = &value
	}
	if r.report.Outcome != nil {
		value := *r.report.Outcome
		report.Outcome = &value
	}

	return report
}

// Write atomically writes the current report.
func (r *Recorder) Write(path string) error {
	if r == nil || path == "" {
		return nil
	}

	report := r.Snapshot()
	data, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("marshal lifecycle report: %w", err)
	}
	data = append(data, '\n')

	tempFile, err := os.CreateTemp(filepath.Dir(path), fmt.Sprintf(".%s.*.tmp", filepath.Base(path)))
	if err != nil {
		return fmt.Errorf("create temporary lifecycle report: %w", err)
	}
	temp := tempFile.Name()
	defer func() { _ = os.Remove(temp) }()

	if _, err := tempFile.Write(data); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("write temporary lifecycle report: %w", err)
	}
	if err := tempFile.Close(); err != nil {
		return fmt.Errorf("close temporary lifecycle report: %w", err)
	}
	if err := os.Rename(temp, path); err != nil {
		return fmt.Errorf("replace lifecycle report: %w", err)
	}

	return nil
}

func errorString(err error) string {
	if err == nil {
		return ""
	}

	return err.Error()
}

func cloneSlice[T any](values []T) []T {
	if values == nil {
		return nil
	}

	cloned := make([]T, len(values))
	copy(cloned, values)

	return cloned
}
