//go:build windows

package leasefile

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func lock(f *os.File) error {
	var ol windows.Overlapped
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &ol)
}

func TryLock(f *os.File) error {
	var ol windows.Overlapped
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &ol)
}

func Unlock(f *os.File) error {
	var ol windows.Overlapped
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &ol)
}

// IsContended reports whether err from TryLock means another holder has the
// lock, as opposed to locking failing outright.
func IsContended(err error) bool {
	return errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}
