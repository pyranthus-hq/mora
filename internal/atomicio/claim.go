package atomicio

import (
	"errors"
	"io"
	"os"
)

// ClaimOptions exposes the platform operations used by ClaimExclusiveDurable.
type ClaimOptions struct {
	Link        func(string, string) error
	Unsupported func(error) bool
}

// ClaimExclusiveDurable publishes an already-fsynced file without replacing an existing destination.
// On volumes without hard links, it copies through the exclusive destination handle,
// preserves the source permissions, and syncs the copy before returning success.
// Readers may observe an incomplete copy while that fallback is in progress.
// The caller owns temp cleanup and must sync the parent directory for name durability.
func ClaimExclusiveDurable(temp, dest string, options ...ClaimOptions) error {
	link, unsupported := os.Link, claimLinkUnsupported
	if len(options) > 0 {
		if options[0].Link != nil {
			link = options[0].Link
		}
		if options[0].Unsupported != nil {
			unsupported = options[0].Unsupported
		}
	}
	err := link(temp, dest)
	if err == nil || errors.Is(err, os.ErrExist) {
		return err
	}
	if !unsupported(err) {
		return err
	}
	claim, createErr := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if createErr != nil {
		return createErr
	}
	// Keep the claimed name present throughout publication. Replacing a closed
	// placeholder can briefly remove dest on Windows, admitting a second winner.
	cleanup := func(err error) error {
		return errors.Join(err, claim.Close(), os.Remove(dest))
	}
	source, err := os.Open(temp)
	if err != nil {
		return cleanup(err)
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return cleanup(err)
	}
	if _, err := io.Copy(claim, source); err != nil {
		return cleanup(err)
	}
	// Chmod explicitly restores permissions masked by the process umask.
	if err := claim.Chmod(info.Mode().Perm()); err != nil {
		return cleanup(err)
	}
	if err := claim.Sync(); err != nil {
		return cleanup(err)
	}
	if err := claim.Close(); err != nil {
		return errors.Join(err, os.Remove(dest))
	}
	return nil
}
