//go:build !windows

package operation

import (
	"errors"
	"os"
	"syscall"
)

// processAlive reports whether a process with the given pid is still running. It is
// the liveness check behind the ingest lease (A3 rule d): a lease left by a SIGKILLed
// ingest names a dead pid, so the next rebuild reclaims it instead of pinning the
// index dirty forever. Signal 0 performs the kernel's existence/permission check
// without delivering a signal. A same-user live process returns nil; a gone process
// returns ESRCH. EPERM means the process exists but belongs to someone else, which
// reads as alive: a wrong "dead" can retire a live owner's marker. Mirrors the syncDir build-tag split the package already ships.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid) // never errors on POSIX
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}
