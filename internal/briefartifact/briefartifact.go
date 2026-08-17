// Package briefartifact owns deterministic, crash-durable dated brief files.
package briefartifact

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/atomicio"
)

// Path returns <vault>/briefs/<UTC-date>-brief.md using the injected time.
func Path(vault string, now time.Time) string {
	return filepath.Join(vault, "briefs", now.UTC().Format("2006-01-02")+"-brief.md")
}

// Latest resolves the highest parseable YYYY-MM-DD brief filename under the vault.
func Latest(vault string) (string, time.Time, bool) {
	dir := filepath.Join(vault, "briefs")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", time.Time{}, false
	}
	var bestName string
	var bestDate time.Time
	found := false
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		prefix, ok := strings.CutSuffix(entry.Name(), "-brief.md")
		if !ok {
			continue
		}
		date, err := time.Parse("2006-01-02", prefix)
		if err != nil {
			continue
		}
		if !found || date.After(bestDate) {
			bestName, bestDate, found = entry.Name(), date, true
		}
	}
	if !found {
		return "", time.Time{}, false
	}
	return filepath.Join(dir, bestName), bestDate.UTC(), true
}

// IsFresh reports whether dated is today or yesterday in now's UTC calendar.
func IsFresh(dated, now time.Time) bool {
	today := now.UTC().Format("2006-01-02")
	yesterday := now.UTC().AddDate(0, 0, -1).Format("2006-01-02")
	date := dated.UTC().Format("2006-01-02")
	return date == today || date == yesterday
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
