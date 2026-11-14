//go:build windows

package managedtree

import (
	"fmt"
	"os"
	"syscall"
)

func unsafeReparse(path string, _ os.FileInfo) (bool, error) {
	attrs, err := syscall.GetFileAttributes(syscall.StringToUTF16Ptr(path))
	if err != nil {
		return true, fmt.Errorf("reading reparse attributes for %s: %w", path, err)
	}
	return attrs&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0, nil
}
