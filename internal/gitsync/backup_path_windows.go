//go:build windows

package gitsync

import "syscall"

func backupReparse(path string) (bool, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return true, err
	}
	attrs, err := syscall.GetFileAttributes(name)
	return attrs&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0, err
}
func backupSystemAlias(string) bool { return false }
