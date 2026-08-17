// Package briefartifact owns deterministic, crash-durable dated brief files.
package briefartifact

import (
	"os"
	"path/filepath"
	"time"

	"github.com/pyranthus-hq/mora/internal/atomicio"
)

// Path returns <vault>/briefs/<UTC-date>-brief.md using the injected time.
func Path(vault string, now time.Time) string {
	return filepath.Join(vault, "briefs", now.UTC().Format("2006-01-02")+"-brief.md")
}

// Write durably replaces the dated artifact with the supplied rendered bytes.
func Write(vault string, now time.Time, body []byte) (string, error) {
	path := Path(vault, now)
	if err := atomicio.WriteDurable(path, body, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// Mode is the human-readable vault-artifact mode.
const Mode os.FileMode = 0o644
