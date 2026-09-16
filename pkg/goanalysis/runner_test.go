package goanalysis

import (
	"errors"
	"testing"
	"time"

	"golang.org/x/tools/go/analysis"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/golangci/golangci-lint/v2/pkg/lint/lifecycle"
	"github.com/golangci/golangci-lint/v2/pkg/result"
)

func TestCollectAnalyzerStats(t *testing.T) {
	alpha := &analysis.Analyzer{Name: "alpha"}
	zeta := &analysis.Analyzer{Name: "zeta"}

	stats := collectAnalyzerStats([]*action{
		{Analyzer: zeta, Duration: 2 * time.Millisecond, Diagnostics: make([]analysis.Diagnostic, 2)},
		{Analyzer: alpha, Duration: time.Millisecond},
		{Analyzer: zeta, Err: errors.New("failed")},
	})

	require.Len(t, stats, 2)
	assert.Equal(t, alpha, stats[0].analyzer)
	assert.Equal(t, time.Millisecond, stats[0].elapsed)
	assert.Equal(t, 1, stats[0].actions)
	assert.Equal(t, 1, stats[0].executedActions)
	assert.Equal(t, zeta, stats[1].analyzer)
	assert.Equal(t, 2*time.Millisecond, stats[1].elapsed)
	assert.Equal(t, 2, stats[1].actions)
	assert.Equal(t, 1, stats[1].executedActions)
	assert.Equal(t, 2, stats[1].diagnostics)
	assert.Equal(t, 1, stats[1].errors)
}

func TestAnalysisLifecycleRecordsPanic(t *testing.T) {
	recorder := lifecycle.NewRecorder()
	metrics := &analysisLifecycle{
		recorder:      recorder,
		started:       time.Now(),
		report:        &lifecycle.AnalysisRun{},
		linterIndexes: map[string]int{},
	}

	assert.PanicsWithError(t, "boom", func() {
		var issues []*result.Issue
		var runErr error
		defer func() { metrics.finishRecovered(issues, runErr, recover()) }()

		panic(errors.New("boom"))
	})

	report := recorder.Snapshot()
	require.Len(t, report.Analysis, 1)
	assert.Equal(t, "boom", report.Analysis[0].Error)
}
