//go:build windows

package operation

import (
	"errors"
	"syscall"
)

const (
	// processQueryLimitedInformation is PROCESS_QUERY_LIMITED_INFORMATION, which
	// the standard syscall package does not name.
	processQueryLimitedInformation = 0x1000
)

// processAlive reports whether a process with the given pid is still running.
// OpenProcess alone cannot tell: it also succeeds for a process that has
// exited while some other handle to it stays open, so a killed run would read
// as alive and its lease or rebuild receipt would never be reclaimed. A process
// object is signalled once it has exited, so a zero-wait on it decides, and a
// process that exited with code 259 is still read as dead. A process that
// exists but refuses the open (another user's) reads as alive, like EPERM on
// POSIX, because a wrong "dead" can retire a live owner's marker. Used by the
// ingest lease so a killed run's lease does not pin the index dirty (A3 rule d).
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := syscall.OpenProcess(processQueryLimitedInformation|syscall.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return errors.Is(err, syscall.ERROR_ACCESS_DENIED)
	}
	defer syscall.CloseHandle(h)
	event, err := syscall.WaitForSingleObject(h, 0)
	if err != nil {
		return true
	}
	return event == syscall.WAIT_TIMEOUT
}
