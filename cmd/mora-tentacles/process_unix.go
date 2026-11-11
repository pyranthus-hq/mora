//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

func cancelProcessTree(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Backup shells spawn Mora; cancellation must not leave an unowned writer behind.
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
