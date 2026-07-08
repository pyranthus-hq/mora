//go:build windows

package mora

import (
	"os"
	"syscall"
	"testing"
)

// TestSharingViolationRetryable_Windows pins the Windows contention classifier
// that makes acquireSourcesLock RETRY (not fatally fail) when a rival writer
// races an os.Remove/os.Link on the same `.lock` — the ERROR_SHARING_VIOLATION /
// ERROR_ACCESS_DENIED that failed build-windows on TestSourcesConcurrentRMWNoLostUpdate.
// Deterministic: it classifies constructed errnos, no race required.
func TestSharingViolationRetryable_Windows(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"sharing-violation on open (os.ReadFile of the lock body)", &os.PathError{Op: "open", Path: `C:\x\sources.json.lock`, Err: errSharingViolation}, true},
		{"access-denied on open", &os.PathError{Op: "open", Path: `C:\x\sources.json.lock`, Err: syscall.ERROR_ACCESS_DENIED}, true},
		{"sharing-violation on rename (os.Rename LinkError)", &os.LinkError{Op: "rename", Old: "a", New: "b", Err: errSharingViolation}, true},
		{"access-denied on link (os.Link)", &os.LinkError{Op: "link", Old: "a", New: "b", Err: syscall.ERROR_ACCESS_DENIED}, true},
		{"file-not-found is NOT contention", &os.PathError{Op: "open", Path: "x", Err: syscall.ERROR_FILE_NOT_FOUND}, false},
		{"nil is NOT contention", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sharingViolationRetryable(tc.err); got != tc.want {
				t.Fatalf("sharingViolationRetryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
			// renameReplaceRetryable delegates to the same classification.
			if got := renameReplaceRetryable(tc.err); got != tc.want {
				t.Fatalf("renameReplaceRetryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
