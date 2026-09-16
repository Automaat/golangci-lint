package commands

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/golangci/golangci-lint/v2/internal/processexit"
	"github.com/golangci/golangci-lint/v2/pkg/exitcodes"
	"github.com/golangci/golangci-lint/v2/pkg/lint/lifecycle"
	"github.com/golangci/golangci-lint/v2/pkg/lint/workerprotocol"
)

const (
	workerEndpointEnv = "GOLANGCI_WORKER_ENDPOINT"
	workerProofEnv    = "GOLANGCI_WORKER_TOKEN"
	workerDialTimeout = 5 * time.Second
	workerReadBuffer  = 4 * 1024
	workerMetricLint  = "linters"
	workerShutdownSeq = 4
)

type workerReadyPayload struct {
	workerprotocol.ReadyPayload
	AuthToken string `json:"auth_token"`
}

// TryExecuteWorker runs one private protocol session when requested by the Rust controller.
func TryExecuteWorker(info BuildInfo) (handled bool, exitCode int, err error) {
	endpoint, enabled := os.LookupEnv(workerEndpointEnv)
	if !enabled {
		return false, 0, nil
	}
	token := os.Getenv(workerProofEnv)
	if token == "" {
		return true, exitcodes.Failure, errors.New("worker authentication token is required")
	}

	address, err := loopbackAddress(endpoint)
	if err != nil {
		return true, exitcodes.Failure, err
	}
	dialer := net.Dialer{Timeout: workerDialTimeout}
	connection, err := dialer.DialContext(context.Background(), "tcp", address)
	if err != nil {
		return true, exitcodes.Failure, fmt.Errorf("connect worker transport: %w", err)
	}
	defer connection.Close()

	exitCode, err = serveWorker(info, connection, token, executeWorkerRun)
	return true, exitCode, err
}

func loopbackAddress(endpoint string) (string, error) {
	host, _, err := net.SplitHostPort(endpoint)
	if err != nil {
		return "", fmt.Errorf("invalid worker endpoint: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return "", errors.New("worker endpoint must use a numeric loopback address")
	}

	return endpoint, nil
}

type workerRunFunc func(BuildInfo, workerprotocol.RunPayload) (lifecycle.Report, int, error)

func serveWorker(info BuildInfo, connection net.Conn, token string, execute workerRunFunc) (int, error) {
	reader := bufio.NewReader(connection)
	decoder := &workerprotocol.Decoder{}
	run, requestID, err := negotiateWorkerRun(connection, reader, decoder, token)
	if err != nil {
		return exitcodes.Failure, err
	}

	report, exitCode, runErr := execute(info, run)
	writeErr := writeWorkerLifecycle(connection, requestID, 2, &report)
	if writeErr != nil {
		return exitcodes.Failure, writeErr
	}
	writeErr = writeWorkerEnvelope(connection, workerprotocol.KindComplete, requestID, 3,
		workerprotocol.CompletePayload{
			ExitCode:     int64(exitCode),
			Issues:       workerIssueCount(&report),
			ElapsedNS:    report.ElapsedNS,
			ContextError: workerContextError(&report, runErr),
		})
	if writeErr != nil {
		return exitcodes.Failure, writeErr
	}

	deadlineErr := connection.SetDeadline(time.Now().Add(workerDialTimeout))
	if deadlineErr != nil {
		return exitcodes.Failure, fmt.Errorf("set worker shutdown deadline: %w", deadlineErr)
	}
	shutdown, err := readWorkerEnvelope(reader, decoder)
	if err != nil {
		return exitcodes.Failure, err
	}
	if shutdown.Kind != workerprotocol.KindShutdown {
		return exitcodes.Failure, fmt.Errorf("worker expected shutdown, got %s", shutdown.Kind)
	}
	if err := writeWorkerEnvelope(connection, workerprotocol.KindShutdownAck, "", workerShutdownSeq,
		workerprotocol.ShutdownPayload{}); err != nil {
		return exitcodes.Failure, err
	}

	return exitCode, runErr
}

func negotiateWorkerRun(connection net.Conn, reader *bufio.Reader, decoder *workerprotocol.Decoder,
	token string,
) (workerprotocol.RunPayload, string, error) {
	deadlineErr := connection.SetDeadline(time.Now().Add(workerDialTimeout))
	if deadlineErr != nil {
		return workerprotocol.RunPayload{}, "", fmt.Errorf("set worker handshake deadline: %w", deadlineErr)
	}
	hello, err := readWorkerEnvelope(reader, decoder)
	if err != nil {
		return workerprotocol.RunPayload{}, "", err
	}
	if hello.Kind != workerprotocol.KindHello {
		return workerprotocol.RunPayload{}, "", fmt.Errorf("worker expected hello, got %s", hello.Kind)
	}
	helloPayload, err := workerprotocol.DecodePayload[workerprotocol.HelloPayload](hello)
	if err != nil {
		return workerprotocol.RunPayload{}, "", err
	}
	if !hasWorkerCapability(helloPayload.Capabilities, "lifecycle") {
		return workerprotocol.RunPayload{}, "", errors.New("worker controller requires lifecycle capability")
	}
	writeErr := writeWorkerEnvelope(connection, workerprotocol.KindReady, "", 1,
		workerReadyPayload{
			ReadyPayload: workerprotocol.ReadyPayload{
				Worker:       "golangci-lint",
				Capabilities: []string{"lifecycle"},
			},
			AuthToken: token,
		})
	if writeErr != nil {
		return workerprotocol.RunPayload{}, "", writeErr
	}

	runEnvelope, err := readWorkerEnvelope(reader, decoder)
	if err != nil {
		return workerprotocol.RunPayload{}, "", err
	}
	if runEnvelope.Kind != workerprotocol.KindRun {
		return workerprotocol.RunPayload{}, "", fmt.Errorf("worker expected run, got %s", runEnvelope.Kind)
	}
	run, err := workerprotocol.DecodePayload[workerprotocol.RunPayload](runEnvelope)
	if err != nil {
		return workerprotocol.RunPayload{}, "", err
	}
	deadlineErr = connection.SetDeadline(time.Time{})
	if deadlineErr != nil {
		return workerprotocol.RunPayload{}, "", fmt.Errorf("clear worker handshake deadline: %w", deadlineErr)
	}

	return run, *runEnvelope.RequestID, nil
}

func hasWorkerCapability(capabilities []string, expected string) bool {
	for _, capability := range capabilities {
		if capability == expected {
			return true
		}
	}

	return false
}

func executeWorkerRun(info BuildInfo, run workerprotocol.RunPayload) (lifecycle.Report, int, error) {
	restore, err := applyWorkerProcessState(run)
	if err != nil {
		return lifecycle.Report{}, exitcodes.Failure, err
	}
	restoreState := true
	defer func() {
		if restoreState {
			restore()
		}
	}()

	recorder := lifecycle.NewRecorder()
	root := newRootCommandWithRunOptions(info, &runCommandOptions{
		lifecycleReportPath: os.Getenv(lifecycle.EnvReportPath),
		lifecycleRecorder:   recorder,
		exitFn:              processexit.Exit,
	})
	if !isWorkerRun(root, run.Args) {
		return lifecycle.Report{}, exitcodes.Failure, errors.New("worker only accepts the run command")
	}
	if setupErr := workerFatalSetupError(); setupErr != nil {
		root.log.Errorf("%s", setupErr)
		recorder.Finish(exitcodes.Failure, setupErr, nil)

		return recorder.Snapshot(), exitcodes.Failure, nil
	}
	root.cmd.SetArgs(run.Args)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root.cmd.SetContext(ctx)
	err = executeWorkerCommand(root.Execute, cancel)
	var processExit workerProcessExit
	if errors.As(err, &processExit) {
		restoreState = false
		if recorder.Snapshot().Outcome == nil {
			recorder.Finish(processExit.code, processExit, nil)
		}

		return recorder.Snapshot(), processExit.code, nil
	}
	exitCode := root.run.exitCode
	if err != nil {
		exitCode = exitcodes.Failure
		if recorder.Snapshot().Outcome == nil {
			recorder.Finish(exitCode, err, nil)
		}
	}

	return recorder.Snapshot(), exitCode, err
}

type workerProcessExit struct {
	code int
}

func (e workerProcessExit) Error() string {
	return fmt.Sprintf("command exited with code %d", e.code)
}

func executeWorkerCommand(execute func() error, cancel context.CancelFunc) error {
	state := struct {
		sync.Mutex
		open bool
	}{open: true}
	terminal := make(chan error, 1)
	claim := func() bool {
		state.Lock()
		defer state.Unlock()
		if !state.open {
			return false
		}
		state.open = false

		return true
	}
	// The worker is single-use; retaining the handler closes races with late async exits.
	processexit.Set(func(code int) {
		if claim() {
			terminal <- workerProcessExit{code: code}
		}
		// Preserve os.Exit's non-returning behavior until the worker process exits.
		select {}
	})

	go func() {
		err := execute()
		if claim() {
			terminal <- err
		}
	}()

	err := <-terminal
	var processExit workerProcessExit
	if errors.As(err, &processExit) {
		cancel()
	}

	return err
}

func workerFatalSetupError() error {
	opts, err := forceRootParsePersistentFlags()
	if err != nil || opts == nil {
		return nil
	}
	if opts.Color != "always" && opts.Color != "auto" && opts.Color != "never" {
		return invalidColorError(opts.Color)
	}

	return nil
}

func isWorkerRun(root *rootCommand, args []string) bool {
	selected, _, err := root.cmd.Find(args)

	return err == nil && selected == root.run.cmd
}

func applyWorkerProcessState(run workerprotocol.RunPayload) (func(), error) {
	for _, private := range []string{workerEndpointEnv, workerProofEnv} {
		if _, exists := run.Environment[private]; exists {
			return nil, fmt.Errorf("worker environment forbids %s", private)
		}
	}
	workingDirectory, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("read worker directory: %w", err)
	}
	originalArgs := os.Args

	keys := make([]string, 0, len(run.Environment)+2)
	for key := range run.Environment {
		keys = append(keys, key)
	}
	keys = append(keys, workerEndpointEnv, workerProofEnv)
	sort.Strings(keys)
	type previousValue struct {
		value   string
		present bool
	}
	previous := make(map[string]previousValue, len(keys))
	for _, key := range keys {
		value, present := os.LookupEnv(key)
		previous[key] = previousValue{value: value, present: present}
	}
	restore := func() {
		for _, key := range keys {
			value := previous[key]
			if value.present {
				_ = os.Setenv(key, value.value)
			} else {
				_ = os.Unsetenv(key)
			}
		}
		os.Args = originalArgs
		_ = os.Chdir(workingDirectory)
	}

	chdirErr := os.Chdir(run.WorkingDirectory)
	if chdirErr != nil {
		return nil, fmt.Errorf("change worker directory: %w", chdirErr)
	}
	os.Args = append([]string{originalArgs[0]}, run.Args...)
	for _, key := range keys {
		if key == workerEndpointEnv || key == workerProofEnv {
			err = os.Unsetenv(key)
		} else {
			err = os.Setenv(key, run.Environment[key])
		}
		if err != nil {
			restore()

			return nil, fmt.Errorf("set worker environment %s: %w", key, err)
		}
	}

	return restore, nil
}

func readWorkerEnvelope(reader *bufio.Reader, decoder *workerprotocol.Decoder) (workerprotocol.Envelope, error) {
	line, err := readWorkerLine(reader)
	if err != nil {
		return workerprotocol.Envelope{}, err
	}
	envelope, err := decoder.DecodeLine(line)
	if err != nil {
		return workerprotocol.Envelope{}, fmt.Errorf("decode worker message: %w", err)
	}

	return envelope, nil
}

func readWorkerLine(reader *bufio.Reader) ([]byte, error) {
	line := make([]byte, 0, workerReadBuffer)
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(line)+len(fragment) > workerprotocol.MaxLineBytes+2 {
			return nil, errors.New("worker message exceeds protocol limit")
		}
		line = append(line, fragment...)
		switch {
		case err == nil:
			return line, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && len(line) != 0:
			return nil, errors.New("worker message is not newline terminated")
		default:
			return nil, fmt.Errorf("read worker message: %w", err)
		}
	}
}

func writeWorkerLifecycle(connection io.Writer, requestID string, sequence uint64, report *lifecycle.Report) error {
	state := "completed"
	contextError := workerContextError(report, nil)
	if contextError != "" {
		state = "failed"
	}
	metrics := map[string]int64{
		"analyses":       int64(len(report.Analysis)),
		"issues":         workerIssueCount(report),
		workerMetricLint: int64(len(report.Linters)),
	}
	if report.PackageLoad != nil {
		metrics["packages"] = int64(report.PackageLoad.DeduplicatedPkgs)
	}

	return writeWorkerEnvelope(connection, workerprotocol.KindLifecycle, requestID, sequence,
		workerprotocol.LifecyclePayload{
			Phase:     "analysis",
			State:     state,
			ElapsedNS: report.ElapsedNS,
			Metrics:   metrics,
			Error:     contextError,
		})
}

func writeWorkerEnvelope(writer io.Writer, kind workerprotocol.Kind, requestID string, sequence uint64, payload any) error {
	envelope, err := workerprotocol.NewEnvelope(kind, requestID, sequence, payload)
	if err != nil {
		return fmt.Errorf("build worker message: %w", err)
	}
	record, err := workerprotocol.Encode(envelope)
	if err != nil {
		return fmt.Errorf("encode worker message: %w", err)
	}
	if _, err := io.Copy(writer, bytes.NewReader(record)); err != nil {
		return fmt.Errorf("write worker message: %w", err)
	}

	return nil
}

func workerIssueCount(report *lifecycle.Report) int64 {
	if report.Processing == nil {
		return 0
	}

	return int64(report.Processing.Output)
}

func workerContextError(report *lifecycle.Report, runErr error) string {
	if report.Outcome != nil {
		if report.Outcome.ContextError != "" {
			return report.Outcome.ContextError
		}
		if report.Outcome.Error != "" {
			return report.Outcome.Error
		}
	}
	if runErr != nil {
		return runErr.Error()
	}

	return ""
}
