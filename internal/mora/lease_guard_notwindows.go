//go:build !windows

package mora

import (
	"os"
	"syscall"
)

func lockLeaseGuard(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

func unlockLeaseGuard(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
