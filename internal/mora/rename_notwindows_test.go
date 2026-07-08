//go:build !windows

package mora

import (
	"errors"
	"os"
	"testing"
)

// TestSharingViolationRetryable_NotWindows pins the off-Windows contract: POSIX
// open(2)/link(2)/unlink(2)/rename(2) never return a transient sharing-violation,
// so the classifier is ALWAYS false and the acquire/rename retry loops run their
// file op exactly once (behavior byte-identical to bare stdlib calls). This runs
// on the Linux CI jobs and locally, guarding against the stub ever returning true.
func TestSharingViolationRetryable_NotWindows(t *testing.T) {
	for _, err := range []error{nil, os.ErrPermission, os.ErrNotExist, errors.New("boom")} {
		if sharingViolationRetryable(err) {
			t.Fatalf("sharingViolationRetryable(%v) = true off Windows; must always be false", err)
		}
		if renameReplaceRetryable(err) {
			t.Fatalf("renameReplaceRetryable(%v) = true off Windows; must always be false", err)
		}
	}
}
