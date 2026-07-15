//go:build windows

package mora

import "os"

// processAlive reports whether a process with the given pid is still running. On
// Windows os.FindProcess calls OpenProcess, which fails for a gone pid, so a
// successful open means the process (or a not-yet-reaped handle) still exists — a
// best-effort liveness check, exactly like the POSIX signal-0 half. Used by the
// ingest lease so a SIGKILLed run's lease does not pin the index dirty (A3 rule d).
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = p.Release()
	return true
}
