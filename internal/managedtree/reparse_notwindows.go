//go:build !windows

package managedtree

import "os"

func unsafeReparse(string, os.FileInfo) (bool, error) { return false, nil }
