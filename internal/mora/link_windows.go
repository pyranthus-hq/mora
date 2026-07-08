//go:build windows

package mora

import (
	"errors"
	"syscall"
)

// errNotSupported is Windows ERROR_NOT_SUPPORTED (50), which CreateHardLinkW
// returns on a volume without hard-link support (FAT32/exFAT). Go's syscall
// package does not export it as a named constant, so declare it locally as a
// syscall.Errno for errors.Is against the *os.LinkError that os.Link returns
// (same pattern as rename_windows.go's errSharingViolation).
const errNotSupported syscall.Errno = 50

// linkUnsupported reports whether a failed os.Link is the volume refusing hard
// links (FAT32/exFAT) rather than a collision (os.ErrExist, handled separately) or
// a real fault. Matches ERROR_NOT_SUPPORTED explicitly plus errors.ErrUnsupported,
// which the syscall package also maps ERROR_INVALID_FUNCTION / ERROR_CALL_NOT_
// IMPLEMENTED onto — other codes some FAT/exFAT drivers report for an unsupported
// operation. Deliberately CONSERVATIVE: ERROR_ACCESS_DENIED and other real faults
// map to ErrPermission, not ErrUnsupported, so they still surface as hard errors.
// Mirrors renameReplaceRetryable's build-tag split.
func linkUnsupported(err error) bool {
	return errors.Is(err, errNotSupported) ||
		errors.Is(err, errors.ErrUnsupported)
}
