//go:build !windows

package mora

import (
	"os"
	"syscall"
)

// processAlive reports whether a process with the given pid is still running. It is
// the liveness check behind the ingest lease (A3 rule d): a lease left by a SIGKILLed
// ingest names a dead pid, so the next rebuild reclaims it instead of pinning the
// index dirty forever. Signal 0 performs the kernel's existence/permission check
// without delivering a signal. A same-user live process returns nil; a gone process
// returns ESRCH. Mirrors the syncDir build-tag split the package already ships.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid) // never errors on POSIX
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
