//go:build !windows

package managedtree

import "os"

func executableMatches(expected bool, mode os.FileMode) bool { return executable(mode) == expected }
