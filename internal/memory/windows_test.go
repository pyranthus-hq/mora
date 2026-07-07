package memory

import (
	"os"
	"runtime"
	"testing"
)

// skipOnWindows skips a test whose failure-injection mechanism is POSIX-only and
// cannot be reproduced portably on Windows (e.g. chmod, which on Windows only
// toggles the read-only attribute and does not make a directory unwritable). The
// behavior under test is correct on Windows; only the Unix-specific provocation
// is unavailable, so the assertion stays fully live on Linux AND macOS.
func skipOnWindows(t *testing.T, reason string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("windows: " + reason)
	}
}

// assertPermUnix asserts an exact Unix permission bit set, but only off Windows,
// where os.FileInfo.Mode().Perm() is ACL-synthesized (0666 for any writable
// file) and can never equal 0600. The production code still writes the correct
// mode; this only relaxes the assertion on Windows.
func assertPermUnix(t *testing.T, got, want os.FileMode) {
	t.Helper()
	if runtime.GOOS != "windows" && got.Perm() != want.Perm() {
		t.Fatalf("mode = %v, want %v", got.Perm(), want.Perm())
	}
}
