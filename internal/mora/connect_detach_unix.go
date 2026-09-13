//go:build !windows

package mora

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func configureDetached(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
func connectPIDAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
func terminateDetached(p *os.Process) error { return p.Signal(syscall.SIGTERM) }
