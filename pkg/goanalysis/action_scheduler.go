package goanalysis

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type actionResult struct {
	action *action
	err    error
}

type actionTask struct {
	action      *action
	execute     func(*action) error
	completedCh chan<- actionResult
}

type actionWorkerPool struct {
	tasks   chan actionTask
	metrics *schedulerMetrics
	wg      sync.WaitGroup
}

func newActionWorkerPool(size int, metrics *schedulerMetrics) *actionWorkerPool {
	pool := &actionWorkerPool{
		tasks:   make(chan actionTask),
		metrics: metrics,
	}
	for range size {
		pool.metrics.actionGoroutineStarted()
		pool.wg.Go(pool.runWorker)
	}
	return pool
}

func (p *actionWorkerPool) runWorker() {
	defer p.metrics.actionGoroutineFinished()
	for task := range p.tasks {
		p.metrics.actionExecutionStarted()
		err := task.execute(task.action)
		p.metrics.actionExecutionFinished()
		task.completedCh <- actionResult{action: task.action, err: err}
	}
}

func (p *actionWorkerPool) close() {
	close(p.tasks)
	p.wg.Wait()
}

type actionGraphExecutor struct {
	ctx           context.Context
	runCtx        context.Context
	stop          context.CancelFunc
	actions       []*action
	workers       *actionWorkerPool
	metrics       *schedulerMetrics
	execute       func(*action) error
	remainingDeps map[*action]int
	dependents    map[*action][]*action
	ready         []*action
	waitStarted   map[*action]time.Time
	completedCh   chan actionResult
	firstErr      error
	running       int
	completed     int
	stopped       bool
}

func newActionGraphExecutor(ctx context.Context, actions []*action, workers *actionWorkerPool,
	metrics *schedulerMetrics, execute func(*action) error,
) *actionGraphExecutor {
	runCtx, stop := context.WithCancel(ctx)
	executor := &actionGraphExecutor{
		ctx:           ctx,
		runCtx:        runCtx,
		stop:          stop,
		actions:       actions,
		workers:       workers,
		metrics:       metrics,
		execute:       execute,
		remainingDeps: make(map[*action]int, len(actions)),
		dependents:    make(map[*action][]*action, len(actions)),
		ready:         make([]*action, 0, len(actions)),
		waitStarted:   make(map[*action]time.Time, len(actions)),
		completedCh:   make(chan actionResult, len(actions)),
	}
	executor.buildGraph()
	return executor
}

func (e *actionGraphExecutor) buildGraph() {
	var graphStarted time.Time
	if e.metrics != nil {
		graphStarted = time.Now()
	}

	for _, act := range e.actions {
		for _, dep := range act.Deps {
			if dep.Package != act.Package {
				continue
			}

			e.remainingDeps[act]++
			e.dependents[dep] = append(e.dependents[dep], act)
		}

		if e.remainingDeps[act] == 0 {
			e.ready = append(e.ready, act)
		} else if e.metrics != nil {
			e.waitStarted[act] = graphStarted
		}
	}
}

func runActionGraph(ctx context.Context, actions []*action, workers *actionWorkerPool, metrics *schedulerMetrics,
	execute func(*action) error,
) error {
	if len(actions) == 0 {
		return nil
	}

	executor := newActionGraphExecutor(ctx, actions, workers, metrics, execute)
	defer executor.stop()

	return executor.run()
}

func (e *actionGraphExecutor) run() error {
	for len(e.ready) > 0 || e.running > 0 {
		e.launchReady()
		if e.running == 0 {
			break
		}

		e.complete(<-e.completedCh)
		if e.runCtx.Err() != nil {
			e.stopped = true
		}
	}

	e.recordOutstandingWait()
	return e.result()
}

func (e *actionGraphExecutor) launchReady() {
	for !e.stopped && len(e.ready) > 0 {
		if e.runCtx.Err() != nil {
			e.stopped = true
			return
		}

		select {
		case e.workers.tasks <- actionTask{action: e.ready[0], execute: e.execute, completedCh: e.completedCh}:
			e.ready = e.ready[1:]
			e.running++

		case result := <-e.completedCh:
			e.complete(result)

		case <-e.runCtx.Done():
			e.stopped = true
		}
	}
}

func (e *actionGraphExecutor) complete(result actionResult) {
	e.running--
	e.completed++

	if result.err != nil && e.firstErr == nil {
		e.firstErr = result.err
		e.stop()
	}
	if e.runCtx.Err() != nil {
		return
	}

	for _, dependent := range e.dependents[result.action] {
		e.remainingDeps[dependent]--
		if e.remainingDeps[dependent] != 0 {
			continue
		}

		if started, ok := e.waitStarted[dependent]; ok {
			e.metrics.addAnalyzerDependencyWait(time.Since(started))
			delete(e.waitStarted, dependent)
		}
		e.ready = append(e.ready, dependent)
	}
}

func (e *actionGraphExecutor) recordOutstandingWait() {
	if e.metrics == nil {
		return
	}

	for _, started := range e.waitStarted {
		e.metrics.addAnalyzerDependencyWait(time.Since(started))
	}
}

func (e *actionGraphExecutor) result() error {
	if e.firstErr != nil || e.ctx.Err() != nil {
		return e.firstErr
	}
	if e.completed == len(e.actions) {
		return nil
	}

	err := fmt.Errorf("action graph has %d unschedulable nodes", len(e.actions)-e.completed)
	for act, remaining := range e.remainingDeps {
		if remaining > 0 {
			act.Err = err
			break
		}
	}
	return err
}
