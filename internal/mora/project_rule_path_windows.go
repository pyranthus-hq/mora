//go:build windows

package mora

import "syscall"

// projectRuleRegularFile rejects every Windows reparse point, including a
// junction-like file target that os.Lstat alone could otherwise follow.
func projectRuleRegularFile(path string) (bool, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return false, err
	}
	attr, err := syscall.GetFileAttributes(name)
	if err != nil {
		return false, err
	}
	return attr&syscall.FILE_ATTRIBUTE_REPARSE_POINT == 0 && attr&syscall.FILE_ATTRIBUTE_DIRECTORY == 0, nil
}
