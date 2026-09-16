//go:build windows

package main

import (
	"fmt"
	"os/exec"
	"slices"

	"github.com/shirou/gopsutil/v4/process"
)

func configureProcessTree(_ *exec.Cmd) {}

func terminateProcessTree(rootPID int32) []int32 {
	return killProcessTreeRecursive(rootPID)
}

func terminateResidualProcesses(marker string) (int, error) {
	return terminateMarkedProcesses(marker)
}

func killProcessTreeRecursive(rootPID int32) []int32 {
	p, err := process.NewProcess(rootPID)
	if err != nil {
		return nil
	}
	running, err := p.IsRunning()
	if err != nil || !running {
		return nil
	}
	result := []int32{rootPID}
	children, _ := p.Children()
	for _, child := range children {
		result = append(result, killProcessTreeRecursive(child.Pid)...)
	}
	_ = p.Kill()

	return result
}

func terminateMarkedProcesses(marker string) (int, error) {
	processes, err := process.Processes()
	if err != nil {
		return 0, fmt.Errorf("list processes after benchmark: %w", err)
	}
	expected := benchmarkRunMarkerEnv + "=" + marker
	marked := make(map[int32]struct{})
	for _, item := range processes {
		environ, envErr := item.Environ()
		if envErr == nil && slices.Contains(environ, expected) {
			marked[item.Pid] = struct{}{}
			_ = item.Kill()
		}
	}
	if len(marked) == 0 {
		return 0, nil
	}
	_ = waitForProcessExit(marked)

	return len(marked), nil
}
