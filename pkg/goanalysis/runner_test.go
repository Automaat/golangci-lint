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

func TestAnalysisLifecycleRecordsSchedulerStats(t *testing.T) {
	recorder := lifecycle.NewRecorder()
	metrics := &analysisLifecycle{
		recorder: recorder,
		report:   &lifecycle.AnalysisRun{},
	}

	metrics.recordStats(&analysisStats{
		scheduler: schedulerStats{
			rootActions:            3,
			horizontalEdges:        4,
			verticalEdges:          5,
			sourceActions:          6,
			sourceLoads:            7,
			exportLoads:            8,
			peakPackageWorkers:     2,
			peakActionGoroutines:   9,
			peakExecutingActions:   2,
			packageDependencyWait:  10 * time.Millisecond,
			analyzerDependencyWait: 11 * time.Millisecond,
		},
	})

	require.NotNil(t, metrics.report.Scheduler)
	assert.Equal(t, 3, metrics.report.Scheduler.RootActions)
	assert.Equal(t, 4, metrics.report.Scheduler.HorizontalEdges)
	assert.Equal(t, 5, metrics.report.Scheduler.VerticalEdges)
	assert.Equal(t, 6, metrics.report.Scheduler.SourceActions)
	assert.Equal(t, 7, metrics.report.Scheduler.SourceLoads)
	assert.Equal(t, 8, metrics.report.Scheduler.ExportLoads)
	assert.Equal(t, 2, metrics.report.Scheduler.PeakPackageWorkers)
	assert.Equal(t, 9, metrics.report.Scheduler.PeakActionGoroutines)
	assert.Equal(t, 2, metrics.report.Scheduler.PeakExecutingActions)
	assert.Equal(t, int64(10*time.Millisecond), metrics.report.Scheduler.PackageDependencyWaitNS)
	assert.Equal(t, int64(11*time.Millisecond), metrics.report.Scheduler.AnalyzerDependencyWaitNS)
}
