//go:build windows

package mora

import (
	"errors"
	"os"
	"syscall"
	"testing"
)

// syntheticLinkUnsupportedErr returns the Windows error CreateHardLinkW reports on
// a volume without hard-link support (FAT32/exFAT), so the portable fallback
// behavior tests can drive atomicCreate's fallback via the linkPublish seam.
func syntheticLinkUnsupportedErr() error { return errNotSupported }

func TestLinkUnsupportedClassificationWindows(t *testing.T) {
	unsupported := []error{
		errNotSupported,
		&os.LinkError{Op: "link", Err: errNotSupported},
	}
	for _, e := range unsupported {
		if !linkUnsupported(e) {
			t.Errorf("linkUnsupported(%v) = false, want true", e)
		}
	}

	// Conservative: a real fault (access denied, not found) or a collision must
	// NOT be classified as unsupported.
	supported := []error{
		nil,
		os.ErrExist,
		syscall.ERROR_ACCESS_DENIED,
		syscall.ERROR_FILE_NOT_FOUND,
		errors.New("some unrelated error"),
	}
	for _, e := range supported {
		if linkUnsupported(e) {
			t.Errorf("linkUnsupported(%v) = true, want false (must not mask a real error)", e)
		}
	}
}
