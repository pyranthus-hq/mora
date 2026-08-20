//go:build windows

package atomicio

import (
	"errors"
	"syscall"
)

// errSharingViolation is Windows ERROR_SHARING_VIOLATION (32). Go's syscall
// package defines ERROR_ACCESS_DENIED but not this one, so declare it locally as
// a syscall.Errno; errors.Is matches it against the *os.LinkError-wrapped errno
// that os.Rename returns.
const errSharingViolation syscall.Errno = 32

// SharingViolationRetryable reports whether a failed file operation — open/read,
// hardlink, remove, OR rename — is a *transient* Windows sharing/lock contention
// error worth retrying. Windows returns ERROR_ACCESS_DENIED (5) or
// ERROR_SHARING_VIOLATION (32) when one handle touches a path another process is
// concurrently creating, deleting, or renaming (a handle briefly open without
// FILE_SHARE_*, e.g. an antivirus scanner, or a racing MoveFileEx). This is
// EXACTLY the contention a file-lease acquire loop sees when rival writers race
// on the same lock file: an os.ReadFile of the lock body can collide with another
// writer's os.Remove/os.Link on that path and surface ERROR_SHARING_VIOLATION.
// The stdlib wraps the errno in an *os.PathError / *os.LinkError whose Unwrap
// exposes the syscall.Errno, so errors.Is matches. A PERMANENT ACCESS_DENIED
// (e.g. a directory target, or a genuinely unwritable dir) never clears — the
// caller's bounded retry budget bounds that case.
func SharingViolationRetryable(err error) bool {
	return errors.Is(err, syscall.ERROR_ACCESS_DENIED) ||
		errors.Is(err, errSharingViolation)
}

// renameReplaceRetryable is the rename-specific spelling used by
// RenameReplaceWithRetry. On Windows os.Rename is
// MoveFileEx(MOVEFILE_REPLACE_EXISTING), which must delete the existing
// target; racing calls return the same two errnos, so the classification is
// identical to SharingViolationRetryable.
func renameReplaceRetryable(err error) bool { return SharingViolationRetryable(err) }
