package lifecycle

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecorderSnapshot(t *testing.T) {
	recorder := NewRecorder()
	recorder.RecordPackageLoad(2*time.Millisecond, 4, 3, nil)
	recorder.RecordLinter("govet", 3*time.Millisecond, 2, errors.New("failed"))
	recorder.RecordAnalysis(&AnalysisRun{
		Name:          "goanalysis_metalinter",
		RequestedPkgs: 3,
		ConfiguredLinters: []ConfiguredLinterRun{
			{Name: "govet", Issues: 1},
		},
		Analyzers: []AnalyzerRun{
			{Name: "printf", Linter: "govet"},
		},
	})
	recorder.RecordProcessing(4*time.Millisecond, 2, 1)
	recorder.Finish(1, errors.New("run failed"), nil)

	report := recorder.Snapshot()
	report.Linters[0].Name = "changed"
	report.PackageLoad.OriginalPackages = 99
	report.Analysis[0].ConfiguredLinters[0].Name = "changed"
	report.Analysis[0].Analyzers[0].Name = "changed"

	assert.Equal(t, SchemaVersion, report.SchemaVersion)
	assert.GreaterOrEqual(t, report.ElapsedNS, int64(0))
	assert.Equal(t, int64(2*time.Millisecond), report.PackageLoad.ElapsedNS)
	assert.Equal(t, 3, report.PackageLoad.DeduplicatedPkgs)
	assert.Equal(t, "failed", report.Linters[0].Error)
	assert.Equal(t, 1, report.Processing.Output)
	assert.Equal(t, "run failed", report.Outcome.Error)

	unchanged := recorder.Snapshot()
	assert.Equal(t, "govet", unchanged.Linters[0].Name)
	assert.Equal(t, 4, unchanged.PackageLoad.OriginalPackages)
	assert.Equal(t, "govet", unchanged.Analysis[0].ConfiguredLinters[0].Name)
	assert.Equal(t, "printf", unchanged.Analysis[0].Analyzers[0].Name)
}

func TestRecorderConcurrent(t *testing.T) {
	recorder := NewRecorder()

	const count = 100
	var wg sync.WaitGroup
	for range count {
		wg.Go(func() {
			recorder.RecordLinter("test", time.Millisecond, 1, nil)
			recorder.RecordAnalysis(&AnalysisRun{Name: "test"})
		})
	}
	wg.Wait()

	report := recorder.Snapshot()
	assert.Len(t, report.Linters, count)
	assert.Len(t, report.Analysis, count)
}

func TestRecorderWrite(t *testing.T) {
	recorder := NewRecorder()
	recorder.Finish(0, nil, nil)

	path := filepath.Join(t.TempDir(), "lifecycle.json")
	require.NoError(t, recorder.Write(path))
	require.NoError(t, recorder.Write(path))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, byte('\n'), data[len(data)-1])

	var report Report
	require.NoError(t, json.Unmarshal(data, &report))
	assert.Equal(t, SchemaVersion, report.SchemaVersion)
	assert.Equal(t, 0, report.Outcome.ExitCode)
	assert.NotNil(t, report.Linters)
	assert.NotNil(t, report.Analysis)

	temporary, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".lifecycle.json.*.tmp"))
	require.NoError(t, err)
	assert.Empty(t, temporary)
}

func TestNilRecorder(t *testing.T) {
	var recorder *Recorder

	recorder.RecordPackageLoad(0, 0, 0, nil)
	recorder.RecordLinter("", 0, 0, nil)
	recorder.RecordAnalysis(&AnalysisRun{})
	recorder.RecordProcessing(0, 0, 0)
	recorder.Finish(0, nil, nil)
	require.NoError(t, recorder.Write(""))
	assert.Equal(t, Report{}, recorder.Snapshot())
}
