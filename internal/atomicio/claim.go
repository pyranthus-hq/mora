package atomicio

import (
	"errors"
	"os"
)

// ClaimOptions exposes the platform operations used by ClaimExclusiveDurable.
type ClaimOptions struct {
	Link        func(string, string) error
	Unsupported func(error) bool
}

// ClaimExclusiveDurable publishes an already-fsynced file without replacing an existing destination.
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
	claim, createErr := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if createErr != nil {
		return createErr
	}
	if closeErr := claim.Close(); closeErr != nil {
		return errors.Join(closeErr, os.Remove(dest))
	}
	return RenameReplaceWithRetry(temp, dest)
}
