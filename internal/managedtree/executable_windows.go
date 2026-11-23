//go:build windows

package managedtree

import "os"

// Windows has no POSIX executable bit. An imported manifest remains authoritative
// for that metadata; fresh Windows digests record false until a host adds a
// platform-neutral execution policy above this filesystem leaf.
func executableMatches(bool, os.FileMode) bool { return true }
