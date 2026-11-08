//go:build !windows

package atomicio

import (
	"errors"
	"os"
	"syscall"
)

func claimExclusiveFallback(temp, dest string) error {
	claim, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := claim.Close(); err != nil {
		return errors.Join(err, os.Remove(dest))
	}
	// POSIX rename preserves both the claimed name and the synced source's mode.
	if err := RenameReplaceWithRetry(temp, dest); err != nil {
		return errors.Join(err, os.Remove(dest))
	}
	return nil
}

func claimLinkUnsupported(err error) bool {
	return errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, errors.ErrUnsupported)
}
