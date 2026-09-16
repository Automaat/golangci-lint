//go:build !windows

package main

import (
	"errors"
	"os/exec"
	"syscall"
)

func configureProcessTree(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateProcessTree(rootPID int32) []int32 {
	processGroup := -int(rootPID)
	err := syscall.Kill(processGroup, 0)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	if err != nil && !errors.Is(err, syscall.EPERM) {
		return nil
	}
	_ = syscall.Kill(processGroup, syscall.SIGKILL)

	return []int32{rootPID}
}

func terminateResidualProcesses(_ string) (int, error) {
	return 0, nil
}
