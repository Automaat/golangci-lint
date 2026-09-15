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
)

const (
	schemaVersion   = 1
	privateDirMode  = 0o750
	privateFileMode = 0o600
)

// Input identifies one captured diagnostic report.
type Input struct {
	Path     string
	ExitCode int
}

// Summary records normalized comparison results.
type Summary struct {
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

type normalizedReport struct {
	data       []byte
	issueCount int
}

// CompareFiles normalizes and compares two captured diagnostic reports.
func CompareFiles(reference, candidate Input, workloadRoot, outputDir string) (Summary, error) {
	referenceData, err := os.ReadFile(reference.Path)
	if err != nil {
		return Summary{}, fmt.Errorf("read reference output: %w", err)
	}
	candidateData, err := os.ReadFile(candidate.Path)
	if err != nil {
		return Summary{}, fmt.Errorf("read candidate output: %w", err)
	}

	referenceReport, err := normalizeReportData(referenceData, workloadRoot)
	if err != nil {
		return Summary{}, fmt.Errorf("normalize reference output: %w", err)
	}
	candidateReport, err := normalizeReportData(candidateData, workloadRoot)
	if err != nil {
		return Summary{}, fmt.Errorf("normalize candidate output: %w", err)
	}

	if err := prepareOutputDir(outputDir); err != nil {
		return Summary{}, err
	}
	if err := writeFile(filepath.Join(outputDir, "reference.normalized.json"), referenceReport.data); err != nil {
		return Summary{}, err
	}
	if err := writeFile(filepath.Join(outputDir, "candidate.normalized.json"), candidateReport.data); err != nil {
		return Summary{}, err
	}

	diagnosticsMatch := bytes.Equal(referenceReport.data, candidateReport.data)
	exitCodesMatch := reference.ExitCode == candidate.ExitCode
	result := Summary{
		SchemaVersion:    schemaVersion,
		Match:            diagnosticsMatch && exitCodesMatch,
		DiagnosticsMatch: diagnosticsMatch,
		ExitCodesMatch:   exitCodesMatch,
		ReferenceExit:    reference.ExitCode,
		CandidateExit:    candidate.ExitCode,
		ReferenceIssues:  referenceReport.issueCount,
		CandidateIssues:  candidateReport.issueCount,
		ReferenceSHA256:  sha256Hex(referenceReport.data),
		CandidateSHA256:  sha256Hex(candidateReport.data),
	}
	if err := writeJSON(filepath.Join(outputDir, "summary.json"), result); err != nil {
		return Summary{}, err
	}
	if !result.Match {
		return result, fmt.Errorf("compatibility mismatch; see %s", outputDir)
	}

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
