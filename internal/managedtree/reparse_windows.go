//go:build windows

package managedtree

import (
	"os"
	"syscall"
)

func unsafeReparse(path string, _ os.FileInfo) bool {
	attrs, err := syscall.GetFileAttributes(syscall.StringToUTF16Ptr(path))
	return err == nil && attrs&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
}
