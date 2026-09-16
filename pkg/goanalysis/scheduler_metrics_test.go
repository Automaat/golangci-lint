package goanalysis

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"golang.org/x/tools/go/packages"
)

func TestCollectSchedulerGraphStats(t *testing.T) {
	pkgA := &packages.Package{PkgPath: "example.com/a"}
	pkgB := &packages.Package{PkgPath: "example.com/b"}
	horizontal := &action{Package: pkgA}
	vertical := &action{Package: pkgB}
	root := &action{Package: pkgA, Deps: []*action{horizontal, vertical}}

	stats := collectSchedulerGraphStats([]*action{root, horizontal, vertical}, []*action{root})

	assert.Equal(t, 1, stats.rootActions)
	assert.Equal(t, 1, stats.horizontalEdges)
	assert.Equal(t, 1, stats.verticalEdges)
}

func TestSchedulerMetrics(t *testing.T) {
	metrics := &schedulerMetrics{}

	metrics.packageWorkerStarted()
	metrics.packageWorkerStarted()
	metrics.packageWorkerFinished()
	metrics.packageWorkerFinished()

	for range 3 {
		metrics.actionGoroutineStarted()
	}
	for range 3 {
		metrics.actionGoroutineFinished()
	}

	metrics.actionExecutionStarted()
	metrics.actionExecutionFinished()
	metrics.addPackageDependencyWait(7 * time.Millisecond)
	metrics.addAnalyzerDependencyWait(11 * time.Millisecond)
	metrics.sourceLoaded()
	metrics.sourceLoaded()
	metrics.exportLoaded()

	stats := schedulerStats{}
	metrics.finish(&stats, []*action{{needAnalyzeSource: true}, {}})

	assert.Equal(t, 1, stats.sourceActions)
	assert.Equal(t, 2, stats.sourceLoads)
	assert.Equal(t, 1, stats.exportLoads)
	assert.Equal(t, 2, stats.peakPackageWorkers)
	assert.Equal(t, 3, stats.peakActionGoroutines)
	assert.Equal(t, 1, stats.peakExecutingActions)
	assert.Equal(t, 7*time.Millisecond, stats.packageDependencyWait)
	assert.Equal(t, 11*time.Millisecond, stats.analyzerDependencyWait)
}

func TestNilSchedulerMetrics(t *testing.T) {
	var metrics *schedulerMetrics

	metrics.packageWorkerStarted()
	metrics.packageWorkerFinished()
	metrics.actionGoroutineStarted()
	metrics.actionGoroutineFinished()
	metrics.actionExecutionStarted()
	metrics.actionExecutionFinished()
	metrics.addPackageDependencyWait(time.Second)
	metrics.addAnalyzerDependencyWait(time.Second)
	metrics.sourceLoaded()
	metrics.exportLoaded()

	stats := schedulerStats{}
	metrics.finish(&stats, nil)
	assert.Equal(t, schedulerStats{}, stats)
}

func TestSchedulerMetricsConcurrent(t *testing.T) {
	const workers = 100

	metrics := &schedulerMetrics{}
	release := make(chan struct{})
	var started sync.WaitGroup
	started.Add(workers)
	var finished sync.WaitGroup
	finished.Add(workers)

	for range workers {
		metrics.actionGoroutineStarted()
		go func() {
			defer finished.Done()
			started.Done()
			<-release
			metrics.addAnalyzerDependencyWait(time.Millisecond)
			metrics.actionGoroutineFinished()
		}()
	}

	started.Wait()
	close(release)
	finished.Wait()

	stats := schedulerStats{}
	metrics.finish(&stats, nil)
	assert.Equal(t, workers, stats.peakActionGoroutines)
	assert.Equal(t, workers*time.Millisecond, stats.analyzerDependencyWait)
}
