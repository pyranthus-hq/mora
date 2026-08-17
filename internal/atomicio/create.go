package atomicio

import (
	"errors"
	"os"
	"path/filepath"
)

// CreateExclusive stages body beside path and publishes it without replacing an existing file.
func CreateExclusive(path string, body []byte, mode os.FileMode, options ...ClaimOptions) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	temp := file.Name()
	defer os.Remove(temp)
	if _, err := file.Write(body); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Chmod(temp, mode); err != nil {
		return err
	}
	link, unsupported := os.Link, claimLinkUnsupported
	if len(options) > 0 {
		if options[0].Link != nil {
			link = options[0].Link
		}
		if options[0].Unsupported != nil {
			unsupported = options[0].Unsupported
		}
	}
	err = link(temp, path)
	if err == nil || errors.Is(err, os.ErrExist) {
		return err
	}
	if !unsupported(err) {
		return err
	}
	claim, claimErr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if claimErr != nil {
		return claimErr
	}
	if closeErr := claim.Close(); closeErr != nil {
		return errors.Join(closeErr, os.Remove(path))
	}
	if renameErr := RenameReplaceWithRetry(temp, path); renameErr != nil {
		return errors.Join(renameErr, os.Remove(path))
	}
	return nil
}
