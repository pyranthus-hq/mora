//go:build windows

package companion

// The registry's cross-process write lock on Windows.
//
// Windows has no flock, but it has something stronger for this purpose: a share
// mode. A handle opened with a share mode of zero cannot be opened again by
// anybody — not by another process, not by this one — until it is closed, and
// the kernel closes it when the process dies. That gives the same two
// properties the flock path relies on and for the same reason: exclusion the
// kernel enforces, and release that does not depend on a live process
// cooperating.
//
// It also removes a hazard the POSIX side has to defend against. A file with an
// open share-mode-zero handle cannot be deleted or renamed over, so the lock
// file cannot be swapped out from under its holder, and stillOwns has nothing
// to check.

import (
	"os"
	"syscall"
	"time"
)

// errSharingViolation is ERROR_SHARING_VIOLATION. Go's syscall package does not
// export it, and it is the error that means "somebody else holds the lock" —
// the one case that must be retried rather than reported.
const errSharingViolation = syscall.Errno(32)

// lockFile is an exclusive lock held by an open handle with no sharing.
type lockFile struct {
	handle syscall.Handle
}

// acquireLock blocks up to timeout for the exclusive lock on path, creating the
// lock file if it does not exist. The file is never removed.
func acquireLock(path string, _ os.FileMode, timeout, poll time.Duration) (*lockFile, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for {
		handle, err := syscall.CreateFile(
			name,
			syscall.GENERIC_READ|syscall.GENERIC_WRITE,
			0, // no sharing: this is the lock
			nil,
			syscall.OPEN_ALWAYS,
			syscall.FILE_ATTRIBUTE_NORMAL,
			0,
		)
		if err == nil {
			return &lockFile{handle: handle}, nil
		}
		if err != errSharingViolation && err != syscall.ERROR_ACCESS_DENIED {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, ErrLocked
		}
		time.Sleep(poll)
	}
}

// stillOwns is always true here. Windows will not let anyone delete or replace
// a file while a handle to it is open, so the swapped-inode case the POSIX
// implementation guards against cannot occur.
func (l *lockFile) stillOwns() bool { return true }

// release drops the lock by closing the handle.
func (l *lockFile) release() error { return syscall.CloseHandle(l.handle) }

// syncDir is a no-op. Windows cannot open a directory as a file for flushing,
// and NTFS orders the metadata write that publishes a rename against the file
// data itself, so there is no separate directory entry to flush.
func syncDir(string) error { return nil }
