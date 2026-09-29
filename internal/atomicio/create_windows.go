//go:build windows

package atomicio

import (
	"os"

	"golang.org/x/sys/windows"
)

func createExclusiveFallback(temp, path string, _ os.FileMode) error {
	from, err := windows.UTF16PtrFromString(temp)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	// Both names are in the same directory. Without REPLACE_EXISTING or
	// COPY_ALLOWED, this moves the complete staged file only if path is absent.
	// There is no placeholder to delete and no copy exposing a partial body.
	// Keep the OS collision error so errors.Is(err, os.ErrExist) identifies losers.
	if err := windows.MoveFileEx(from, to, 0); err != nil {
		return &os.LinkError{Op: "rename", Old: temp, New: path, Err: err}
	}
	return nil
}
