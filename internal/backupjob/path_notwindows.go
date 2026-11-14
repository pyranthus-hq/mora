//go:build !windows

package backupjob

import "os"

func unsafeReparse(string, os.FileInfo) (bool, error) { return false, nil }
func unsafeMode(mode os.FileMode) bool                { return mode&os.ModeSymlink != 0 || mode&os.ModeIrregular != 0 }
