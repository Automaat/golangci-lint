package commands

import (
	"bufio"
	"net"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/golangci/golangci-lint/v2/pkg/lint/lifecycle"
	"github.com/golangci/golangci-lint/v2/pkg/lint/workerprotocol"
)

func TestServeWorker(t *testing.T) {
	controller, worker := net.Pipe()
	t.Cleanup(func() {
		_ = controller.Close()
		_ = worker.Close()
	})

	result := make(chan struct {
		exitCode int
		err      error
	}, 1)
	go func() {
		exitCode, err := serveWorker(BuildInfo{}, worker, "test-token",
			func(_ BuildInfo, run workerprotocol.RunPayload) (lifecycle.Report, int, error) {
				assert.Equal(t, []string{"run", "./..."}, run.Args)

				return lifecycle.Report{
					ElapsedNS:  42,
					Processing: &lifecycle.Processing{Input: 3, Output: 2},
					Outcome:    &lifecycle.Outcome{ExitCode: 1},
				}, 1, nil
			})
		result <- struct {
			exitCode int
			err      error
		}{exitCode: exitCode, err: err}
	}()

	require.NoError(t, writeWorkerEnvelope(controller, workerprotocol.KindHello, "", 0,
		workerprotocol.HelloPayload{
			Client: "test-controller", Capabilities: []string{"lifecycle"},
		}))

	decoder := &workerprotocol.Decoder{}
	reader := newWorkerTestReader(controller)
	ready, err := readWorkerEnvelope(reader, decoder)
	require.NoError(t, err)
	assert.Equal(t, workerprotocol.KindReady, ready.Kind)
	assert.Equal(t, uint64(1), *ready.Sequence)
	readyPayload, err := workerprotocol.DecodePayload[workerReadyPayload](ready)
	require.NoError(t, err)
	assert.Equal(t, "test-token", readyPayload.AuthToken)

	require.NoError(t, writeWorkerEnvelope(controller, workerprotocol.KindRun, "run-1", 0,
		workerprotocol.RunPayload{
			Args:             []string{"run", "./..."},
			WorkingDirectory: "/workspace",
			Environment:      map[string]string{},
		}))

	lifecycleEnvelope, err := readWorkerEnvelope(reader, decoder)
	require.NoError(t, err)
	assert.Equal(t, workerprotocol.KindLifecycle, lifecycleEnvelope.Kind)
	assert.Equal(t, "run-1", *lifecycleEnvelope.RequestID)
	assert.Equal(t, uint64(2), *lifecycleEnvelope.Sequence)

	completeEnvelope, err := readWorkerEnvelope(reader, decoder)
	require.NoError(t, err)
	assert.Equal(t, workerprotocol.KindComplete, completeEnvelope.Kind)
	complete, err := workerprotocol.DecodePayload[workerprotocol.CompletePayload](completeEnvelope)
	require.NoError(t, err)
	assert.Equal(t, int64(1), complete.ExitCode)
	assert.Equal(t, int64(2), complete.Issues)
	assert.Equal(t, int64(42), complete.ElapsedNS)

	require.NoError(t, writeWorkerEnvelope(controller, workerprotocol.KindShutdown, "", 0,
		workerprotocol.ShutdownPayload{}))
	shutdownAck, err := readWorkerEnvelope(reader, decoder)
	require.NoError(t, err)
	assert.Equal(t, workerprotocol.KindShutdownAck, shutdownAck.Kind)
	assert.Equal(t, uint64(4), *shutdownAck.Sequence)
	workerResult := <-result
	require.NoError(t, workerResult.err)
	assert.Equal(t, 1, workerResult.exitCode)
}

func TestLoopbackAddress(t *testing.T) {
	address, err := loopbackAddress("127.0.0.1:1234")
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:1234", address)

	_, err = loopbackAddress("example.com:1234")
	require.Error(t, err)
	_, err = loopbackAddress("missing-port")
	require.Error(t, err)
}

func TestIsWorkerRun(t *testing.T) {
	for name, test := range map[string]struct {
		args     []string
		expected bool
	}{
		"run":           {args: []string{"run", "./..."}, expected: true},
		"global flag":   {args: []string{"--color=never", "run", "./..."}, expected: true},
		"version":       {args: []string{"version"}, expected: false},
		"run as an arg": {args: []string{"help", "run"}, expected: false},
	} {
		t.Run(name, func(t *testing.T) {
			root := newRootCommandWithRunOptions(BuildInfo{}, &runCommandOptions{})
			assert.Equal(t, test.expected, isWorkerRun(root, test.args))
		})
	}
}

func TestWorkerFatalSetupError(t *testing.T) {
	originalArgs := os.Args
	t.Cleanup(func() { os.Args = originalArgs })
	os.Args = []string{originalArgs[0], "run", "--color=invalid", "./..."}

	err := workerFatalSetupError()
	require.EqualError(t, err, "invalid value \"invalid\" for --color; must be 'always', 'auto', or 'never'")
}

func TestApplyWorkerProcessState(t *testing.T) {
	originalDirectory, err := os.Getwd()
	require.NoError(t, err)
	originalArgs := append([]string(nil), os.Args...)
	t.Setenv(workerEndpointEnv, "127.0.0.1:1234")
	t.Setenv(workerProofEnv, "secret")
	t.Setenv("GOLANGCI_WORKER_TEST", "old")

	restore, err := applyWorkerProcessState(workerprotocol.RunPayload{
		Args:             []string{"run", "./..."},
		WorkingDirectory: t.TempDir(),
		Environment:      map[string]string{"GOLANGCI_WORKER_TEST": "new"},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{originalArgs[0], "run", "./..."}, os.Args)
	_, enabled := os.LookupEnv(workerEndpointEnv)
	assert.False(t, enabled)
	_, enabled = os.LookupEnv(workerProofEnv)
	assert.False(t, enabled)
	assert.Equal(t, "new", os.Getenv("GOLANGCI_WORKER_TEST"))

	restore()
	directory, err := os.Getwd()
	require.NoError(t, err)
	assert.Equal(t, originalDirectory, directory)
	assert.Equal(t, originalArgs, os.Args)
	assert.Equal(t, "127.0.0.1:1234", os.Getenv(workerEndpointEnv))
	assert.Equal(t, "secret", os.Getenv(workerProofEnv))
	assert.Equal(t, "old", os.Getenv("GOLANGCI_WORKER_TEST"))
}

func TestApplyWorkerProcessStateRejectsPrivateEnvironment(t *testing.T) {
	_, err := applyWorkerProcessState(workerprotocol.RunPayload{
		Args:             []string{"run"},
		WorkingDirectory: t.TempDir(),
		Environment:      map[string]string{workerEndpointEnv: "127.0.0.1:1"},
	})
	require.Error(t, err)
}

func newWorkerTestReader(connection net.Conn) *bufio.Reader {
	return bufio.NewReader(connection)
}
