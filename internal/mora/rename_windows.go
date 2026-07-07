//go:build windows

package mora

import (
	"errors"
	"syscall"
)

// errSharingViolation is Windows ERROR_SHARING_VIOLATION (32). Go's syscall
// package defines ERROR_ACCESS_DENIED but not this one, so declare it locally as
// a syscall.Errno; errors.Is matches it against the *os.LinkError-wrapped errno
// that os.Rename returns.
const errSharingViolation syscall.Errno = 32

// renameReplaceRetryable reports whether a failed os.Rename onto an existing
// target is a *transient* Windows sharing/lock error worth retrying. On Windows
// os.Rename is MoveFileEx(MOVEFILE_REPLACE_EXISTING), which must delete the
// existing target; racing MoveFileEx calls (or a handle briefly open without
// FILE_SHARE_DELETE, e.g. an antivirus scanner) return ERROR_ACCESS_DENIED (5)
// or ERROR_SHARING_VIOLATION (32). os.Rename wraps the errno in an *os.LinkError
// whose Unwrap exposes the syscall.Errno, so errors.Is matches. A permanent
// error (e.g. a directory target, which also reports ACCESS_DENIED) never
// clears — the bounded retry cap in atomicWrite bounds that case.
func renameReplaceRetryable(err error) bool {
	return errors.Is(err, syscall.ERROR_ACCESS_DENIED) ||
		errors.Is(err, errSharingViolation)
}
