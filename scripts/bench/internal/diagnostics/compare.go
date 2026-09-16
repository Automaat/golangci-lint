package diagnostics

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/golangci/golangci-lint/v2/scripts/bench/internal/changes"
)

const (
	schemaVersion   = 1
	privateDirMode  = 0o750
	privateFileMode = 0o600
)

// Input identifies one captured diagnostic report.
type Input struct {
	Path          string
	Root          string
	ExitCode      int
	Changes       *changes.Set
	ExpectedMatch *bool
	WorktreePath  string
}

// Summary records normalized comparison results.
type Summary struct {
	SchemaVersion          int    `json:"schema_version"`
	Match                  bool   `json:"match"`
	DiagnosticsMatch       bool   `json:"diagnostics_match"`
	ExitCodesMatch         bool   `json:"exit_codes_match"`
	ChangesCompared        bool   `json:"changes_compared"`
	ChangesMatch           bool   `json:"changes_match"`
	ExpectedCompared       bool   `json:"expected_compared"`
	ReferenceExpectedMatch bool   `json:"reference_expected_match"`
	CandidateExpectedMatch bool   `json:"candidate_expected_match"`
	ReferenceExit          int    `json:"reference_exit_code"`
	CandidateExit          int    `json:"candidate_exit_code"`
	ReferenceIssues        int    `json:"reference_issue_count"`
	CandidateIssues        int    `json:"candidate_issue_count"`
	ReferenceSHA256        string `json:"reference_sha256"`
	CandidateSHA256        string `json:"candidate_sha256"`
	ReferenceChangesSHA256 string `json:"reference_changes_sha256,omitempty"`
	CandidateChangesSHA256 string `json:"candidate_changes_sha256,omitempty"`
	ReferenceWorktree      string `json:"reference_worktree,omitempty"`
	CandidateWorktree      string `json:"candidate_worktree,omitempty"`
}

type normalizedReport struct {
	data       []byte
	issueCount int
}

type changesComparison struct {
	compared     bool
	match        bool
	referenceSHA string
	candidateSHA string
}

type expectedComparison struct {
	compared       bool
	referenceMatch bool
	candidateMatch bool
}

// CompareFiles normalizes and compares two captured diagnostic reports.
func CompareFiles(reference, candidate Input, outputDir string) (Summary, error) {
	if reference.Root == "" || candidate.Root == "" {
		return Summary{}, errors.New("both workload roots are required")
	}
	referenceReport, err := loadNormalizedReport(reference, "reference")
	if err != nil {
		return Summary{}, err
	}
	candidateReport, err := loadNormalizedReport(candidate, "candidate")
	if err != nil {
		return Summary{}, err
	}

	if writeErr := writeNormalizedReports(outputDir, referenceReport, candidateReport); writeErr != nil {
		return Summary{}, writeErr
	}
	changeResult, err := compareChangeSets(reference, candidate, outputDir)
	if err != nil {
		return Summary{}, err
	}
	expectedResult, err := compareExpectedResults(reference, candidate)
	if err != nil {
		return Summary{}, err
	}

	diagnosticsMatch := bytes.Equal(referenceReport.data, candidateReport.data)
	exitCodesMatch := reference.ExitCode == candidate.ExitCode
	result := Summary{
		SchemaVersion: schemaVersion,
		Match: diagnosticsMatch && exitCodesMatch && changeResult.match &&
			expectedResult.referenceMatch && expectedResult.candidateMatch,
		DiagnosticsMatch:       diagnosticsMatch,
		ExitCodesMatch:         exitCodesMatch,
		ChangesCompared:        changeResult.compared,
		ChangesMatch:           changeResult.match,
		ExpectedCompared:       expectedResult.compared,
		ReferenceExpectedMatch: expectedResult.referenceMatch,
		CandidateExpectedMatch: expectedResult.candidateMatch,
		ReferenceExit:          reference.ExitCode,
		CandidateExit:          candidate.ExitCode,
		ReferenceIssues:        referenceReport.issueCount,
		CandidateIssues:        candidateReport.issueCount,
		ReferenceSHA256:        sha256Hex(referenceReport.data),
		CandidateSHA256:        sha256Hex(candidateReport.data),
		ReferenceChangesSHA256: changeResult.referenceSHA,
		CandidateChangesSHA256: changeResult.candidateSHA,
	}
	if !result.Match {
		result.ReferenceWorktree = reference.WorktreePath
		result.CandidateWorktree = candidate.WorktreePath
	}
	if writeErr := writeJSON(filepath.Join(outputDir, "summary.json"), result); writeErr != nil {
		return Summary{}, writeErr
	}
	if !result.Match {
		return result, fmt.Errorf("compatibility mismatch; see %s", outputDir)
	}

	return result, nil
}

func loadNormalizedReport(input Input, label string) (normalizedReport, error) {
	data, err := os.ReadFile(input.Path)
	if err != nil {
		return normalizedReport{}, fmt.Errorf("read %s output: %w", label, err)
	}
	report, err := normalizeReportData(data, input.Root)
	if err != nil {
		return normalizedReport{}, fmt.Errorf("normalize %s output: %w", label, err)
	}

	return report, nil
}

func writeNormalizedReports(outputDir string, reference, candidate normalizedReport) error {
	if err := prepareOutputDir(outputDir); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(outputDir, "reference.normalized.json"), reference.data); err != nil {
		return err
	}

	return writeFile(filepath.Join(outputDir, "candidate.normalized.json"), candidate.data)
}

func compareChangeSets(reference, candidate Input, outputDir string) (changesComparison, error) {
	result := changesComparison{
		compared: reference.Changes != nil || candidate.Changes != nil,
		match:    true,
	}
	if !result.compared {
		return result, nil
	}
	if reference.Changes == nil || candidate.Changes == nil {
		return changesComparison{}, errors.New("both change sets are required")
	}

	referenceData, err := changes.Marshal(*reference.Changes)
	if err != nil {
		return changesComparison{}, fmt.Errorf("marshal reference changes: %w", err)
	}
	candidateData, err := changes.Marshal(*candidate.Changes)
	if err != nil {
		return changesComparison{}, fmt.Errorf("marshal candidate changes: %w", err)
	}
	if writeErr := writeFile(filepath.Join(outputDir, "reference.changes.json"), referenceData); writeErr != nil {
		return changesComparison{}, writeErr
	}
	if writeErr := writeFile(filepath.Join(outputDir, "candidate.changes.json"), candidateData); writeErr != nil {
		return changesComparison{}, writeErr
	}

	result.match = changes.EqualSets(*reference.Changes, *candidate.Changes)
	result.referenceSHA = sha256Hex(referenceData)
	result.candidateSHA = sha256Hex(candidateData)

	return result, nil
}

func compareExpectedResults(reference, candidate Input) (expectedComparison, error) {
	result := expectedComparison{
		compared:       reference.ExpectedMatch != nil || candidate.ExpectedMatch != nil,
		referenceMatch: true,
		candidateMatch: true,
	}
	if !result.compared {
		return result, nil
	}
	if reference.ExpectedMatch == nil || candidate.ExpectedMatch == nil {
		return expectedComparison{}, errors.New("both expected results are required")
	}
	result.referenceMatch = *reference.ExpectedMatch
	result.candidateMatch = *candidate.ExpectedMatch

	return result, nil
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
	if err := sortSuggestedFixes(root); err != nil {
		return normalizedReport{}, err
	}
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

	return normalizedReport{data: normalized, issueCount: issues}, nil
}

func sortSuggestedFixes(root map[string]any) error {
	issuesValue, exists := root["Issues"]
	if !exists || issuesValue == nil {
		return nil
	}
	issues, ok := issuesValue.([]any)
	if !ok {
		return errors.New("issues must be an array")
	}
	for _, item := range issues {
		issue, ok := item.(map[string]any)
		if !ok {
			return errors.New("issues must contain objects")
		}
		if _, err := sortObjectArray(issue, "SuggestedFixes"); err != nil {
			return err
		}
	}

	return nil
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
