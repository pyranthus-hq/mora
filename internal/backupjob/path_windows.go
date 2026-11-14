//go:build windows

package backupjob

import (
	"fmt"
	"os"
	"syscall"
)

func unsafeReparse(path string, _ os.FileInfo) (bool, error) {
	attrs, err := syscall.GetFileAttributes(syscall.StringToUTF16Ptr(path))
	if err != nil {
		return true, fmt.Errorf("reading reparse attributes: %w", err)
	}
	return attrs&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0, nil
}
func unsafeMode(mode os.FileMode) bool { return mode&os.ModeSymlink != 0 || mode&os.ModeIrregular != 0 }
