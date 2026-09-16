package goanalysis

import (
	"sync/atomic"
	"time"
)

type schedulerMetrics struct {
	packageWorkers           atomic.Int64
	peakPackageWorkers       atomic.Int64
	actionGoroutines         atomic.Int64
	peakActionGoroutines     atomic.Int64
	executingActions         atomic.Int64
	peakExecutingActions     atomic.Int64
	packageDependencyWaitNS  atomic.Int64
	analyzerDependencyWaitNS atomic.Int64
	sourceLoads              atomic.Int64
	exportLoads              atomic.Int64
}

type schedulerStats struct {
	rootActions            int
	horizontalEdges        int
	verticalEdges          int
	sourceActions          int
	sourceLoads            int
	exportLoads            int
	peakPackageWorkers     int
	peakActionGoroutines   int
	peakExecutingActions   int
	packageDependencyWait  time.Duration
	analyzerDependencyWait time.Duration
}

func (m *schedulerMetrics) packageWorkerStarted() {
	if m == nil {
		return
	}

	updatePeak(&m.peakPackageWorkers, m.packageWorkers.Add(1))
}

func (m *schedulerMetrics) packageWorkerFinished() {
	if m != nil {
		m.packageWorkers.Add(-1)
	}
}

func (m *schedulerMetrics) actionGoroutineStarted() {
	if m == nil {
		return
	}

	updatePeak(&m.peakActionGoroutines, m.actionGoroutines.Add(1))
}

func (m *schedulerMetrics) actionGoroutineFinished() {
	if m != nil {
		m.actionGoroutines.Add(-1)
	}
}

func (m *schedulerMetrics) actionExecutionStarted() {
	if m == nil {
		return
	}

	updatePeak(&m.peakExecutingActions, m.executingActions.Add(1))
}

func (m *schedulerMetrics) actionExecutionFinished() {
	if m != nil {
		m.executingActions.Add(-1)
	}
}

func (m *schedulerMetrics) addPackageDependencyWait(elapsed time.Duration) {
	if m != nil {
		m.packageDependencyWaitNS.Add(elapsed.Nanoseconds())
	}
}

func (m *schedulerMetrics) addAnalyzerDependencyWait(elapsed time.Duration) {
	if m != nil {
		m.analyzerDependencyWaitNS.Add(elapsed.Nanoseconds())
	}
}

func (m *schedulerMetrics) sourceLoaded() {
	if m != nil {
		m.sourceLoads.Add(1)
	}
}

func (m *schedulerMetrics) exportLoaded() {
	if m != nil {
		m.exportLoads.Add(1)
	}
}

func collectSchedulerGraphStats(actions, roots []*action) schedulerStats {
	stats := schedulerStats{rootActions: len(roots)}
	for _, act := range actions {
		for _, dep := range act.Deps {
			if dep.Package == act.Package {
				stats.horizontalEdges++
			} else {
				stats.verticalEdges++
			}
		}
	}
	return stats
}

func (m *schedulerMetrics) finish(stats *schedulerStats, actions []*action) {
	if m == nil {
		return
	}

	for _, act := range actions {
		if act.needAnalyzeSource {
			stats.sourceActions++
		}
	}
	stats.sourceLoads = int(m.sourceLoads.Load())
	stats.exportLoads = int(m.exportLoads.Load())
	stats.peakPackageWorkers = int(m.peakPackageWorkers.Load())
	stats.peakActionGoroutines = int(m.peakActionGoroutines.Load())
	stats.peakExecutingActions = int(m.peakExecutingActions.Load())
	stats.packageDependencyWait = time.Duration(m.packageDependencyWaitNS.Load())
	stats.analyzerDependencyWait = time.Duration(m.analyzerDependencyWaitNS.Load())
}

func updatePeak(peak *atomic.Int64, current int64) {
	for {
		previous := peak.Load()
		if current <= previous || peak.CompareAndSwap(previous, current) {
			return
		}
	}
}
