package goanalysis

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"golang.org/x/tools/go/packages"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunActionGraphBoundsReadyActions(t *testing.T) {
	pkg := &packages.Package{}
	first := &action{Package: pkg}
	second := &action{Package: pkg}
	joined := &action{Package: pkg, Deps: []*action{first, second}}
	tail := &action{Package: pkg, Deps: []*action{joined}}

	started := make(chan *action, 4)
	release := make(chan struct{})
	var firstDone atomic.Bool
	var secondDone atomic.Bool

	result := make(chan error, 1)
	workers := newActionWorkerPool(2, &schedulerMetrics{})
	defer workers.close()
	go func() {
		result <- runActionGraph(context.Background(), []*action{first, second, joined, tail},
			workers, &schedulerMetrics{}, func(act *action) error {
				started <- act
				switch act {
				case first:
					<-release
					firstDone.Store(true)
				case second:
					<-release
					secondDone.Store(true)
				case joined:
					if !firstDone.Load() || !secondDone.Load() {
						return errors.New("joined action ran before its dependencies")
					}
				}
				return nil
			})
	}()

	assert.ElementsMatch(t, []*action{first, second}, []*action{<-started, <-started})
	assert.Empty(t, started)

	close(release)
	require.NoError(t, <-result)
	assert.ElementsMatch(t, []*action{joined, tail}, []*action{<-started, <-started})
}

func TestRunActionGraphStopsAfterError(t *testing.T) {
	pkg := &packages.Package{}
	failing := &action{Package: pkg}
	dependent := &action{Package: pkg, Deps: []*action{failing}}
	wantErr := errors.New("failed")
	var executed atomic.Int64
	workers := newActionWorkerPool(1, nil)
	defer workers.close()

	err := runActionGraph(context.Background(), []*action{failing, dependent}, workers, nil,
		func(act *action) error {
			executed.Add(1)
			if act == failing {
				return wantErr
			}
			return nil
		})

	require.ErrorIs(t, err, wantErr)
	assert.Equal(t, int64(1), executed.Load())
}

func TestRunActionGraphStopsAfterCancellation(t *testing.T) {
	pkg := &packages.Package{}
	first := &action{Package: pkg}
	second := &action{Package: pkg}
	started := make(chan struct{})
	release := make(chan struct{})
	var executed atomic.Int64

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	workers := newActionWorkerPool(1, nil)
	defer workers.close()
	go func() {
		result <- runActionGraph(ctx, []*action{first, second}, workers, nil,
			func(*action) error {
				executed.Add(1)
				close(started)
				<-release
				return nil
			})
	}()

	<-started
	cancel()
	close(release)

	require.NoError(t, <-result)
	assert.Equal(t, int64(1), executed.Load())
}

func TestRunActionGraphRejectsCycle(t *testing.T) {
	pkg := &packages.Package{}
	first := &action{Package: pkg}
	second := &action{Package: pkg, Deps: []*action{first}}
	first.Deps = []*action{second}
	workers := newActionWorkerPool(1, nil)
	defer workers.close()

	err := runActionGraph(context.Background(), []*action{first, second}, workers, nil,
		func(*action) error { return nil })

	require.EqualError(t, err, "action graph has 2 unschedulable nodes")
	assert.True(t, first.Err != nil || second.Err != nil)
}
