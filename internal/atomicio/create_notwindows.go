//go:build !windows

package atomicio

import (
	"errors"
	"os"
)

func createExclusiveFallback(temp, path string, mode os.FileMode) error {
	claim, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if err := claim.Close(); err != nil {
		return errors.Join(err, os.Remove(path))
	}
	// POSIX rename keeps the destination present throughout publication and
	// carries the staged file's explicit mode, independent of the process umask.
	if err := RenameReplaceWithRetry(temp, path); err != nil {
		return errors.Join(err, os.Remove(path))
	}
	return nil
}
