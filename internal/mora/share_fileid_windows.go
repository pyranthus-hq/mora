//go:build windows

package mora

import "os"

// fileIDKey identifies a file on Windows by (volume serial, file index). Go's
// os.FileInfo does not expose these portably without a handle open, so the
// accountant conservatively skips hard-link dedup on Windows (a hard link is
// counted once per name — an over-count that only makes admission stricter,
// never an under-count, so the fail-closed bound holds).
type fileIDKey struct {
	a uint64
	b uint64
}

func fileIdentity(os.FileInfo) (fileIDKey, bool) { return fileIDKey{}, false }
