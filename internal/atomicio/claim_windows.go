//go:build windows

package atomicio

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

func claimExclusiveFallback(temp, dest string) error {
	// Preserve ErrExist even when the caller's staged source is already gone.
	if _, err := os.Lstat(dest); err == nil {
		return os.ErrExist
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	from, err := windows.UTF16PtrFromString(temp)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(dest)
	if err != nil {
		return err
	}
	// No REPLACE_EXISTING or COPY_ALLOWED: publish the complete synced file only
	// if the destination is absent, without a placeholder or visible partial copy.
	if err := windows.MoveFileEx(from, to, 0); err != nil {
		return &os.LinkError{Op: "rename", Old: temp, New: dest, Err: err}
	}
	return nil
}

const errClaimNotSupported syscall.Errno = 50

func claimLinkUnsupported(err error) bool {
	return errors.Is(err, errClaimNotSupported) || errors.Is(err, errors.ErrUnsupported)
}
