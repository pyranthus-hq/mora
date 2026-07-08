//go:build !windows

package mora

import (
	"errors"
	"os"
	"syscall"
	"testing"
)

// syntheticLinkUnsupportedErr returns a platform-appropriate error that
// linkUnsupported must classify as "hard links unsupported", so the portable
// fallback behavior tests can drive atomicCreate's fallback on an ordinary
// filesystem via the linkPublish seam. On POSIX, link(2) on a no-hardlink
// filesystem (exFAT/FAT32) returns EPERM.
func syntheticLinkUnsupportedErr() error { return syscall.EPERM }

func TestLinkUnsupportedClassificationPOSIX(t *testing.T) {
	// Hard-link-unsupported errnos → fallback (true), including the *os.LinkError
	// shape os.Link actually returns.
	unsupported := []error{
		syscall.EPERM,
		syscall.ENOTSUP,
		syscall.EOPNOTSUPP,
		&os.LinkError{Op: "link", Err: syscall.EPERM},
	}
	for _, e := range unsupported {
		if !linkUnsupported(e) {
			t.Errorf("linkUnsupported(%v) = false, want true", e)
		}
	}

	// Everything else must NOT be classified as unsupported — a real fault has to
	// surface as a hard error, and a collision (EEXIST) is handled before this.
	supported := []error{
		nil,
		os.ErrExist,
		syscall.EACCES,
		syscall.ENOSPC,
		syscall.ENOENT,
		syscall.EROFS,
		errors.New("some unrelated error"),
		&os.LinkError{Op: "link", Err: syscall.EEXIST},
	}
	for _, e := range supported {
		if linkUnsupported(e) {
			t.Errorf("linkUnsupported(%v) = true, want false (must not mask a real error)", e)
		}
	}
}
