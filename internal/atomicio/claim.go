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
// Without hard links, Windows uses a no-replace move of the synced file; Unix
// atomically renames it over an exclusive placeholder. Neither copies a visible
// partial body. The Unix placeholder may remain empty after a crash.
// The fallback consumes temp. The caller must sync the parent directory for name durability.
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
	return claimExclusiveFallback(temp, dest)
}
