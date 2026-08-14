package ingest

import (
	"path/filepath"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/config"
	"github.com/pyranthus-hq/mora/internal/google"
	"github.com/pyranthus-hq/mora/internal/memory"
)

const DefaultIMessageLookbackDays = 365

func OperationSourceKey(s memory.Source) string {
	account := s.Account
	if s.Type == "filesystem" {
		account = s.Name
	}
	return SourceKey(s.Type, account)
}

func GoogleWindow(s memory.Source, kind google.ItemKind, now time.Time) google.FetchWindow {
	w := google.FetchWindow{Labels: s.LabelIDs, CalendarID: s.Calendar}
	switch kind {
	case google.KindGmailThread:
		// Default to a lean 90-day window: a year of mail is mostly low-signal
		// noise for a memory index (~6.7k threads vs ~1.6k here). Override with
		// `mora connect google --since-days N` (persisted on the source, so
		// future `sync google` reuses it).
		days := s.SinceDays
		if days == 0 {
			days = 90
		}
		w.Since = now.AddDate(0, 0, -days)
	case google.KindCalEvent:
		w.Since = now.AddDate(0, -6, 0)
		w.Until = now.AddDate(0, 3, 0)
	}
	return w
}

func GoogleTokenPath(cfg config.Config, account string) string {
	name := "google.json"
	if account != "" {
		name = "google-" + account + ".json"
	}
	return filepath.Join(cfg.ConfigDir, "tokens", name)
}

func IMessageLookbackDays(s memory.Source) int {
	if s.SinceDays == 0 {
		return DefaultIMessageLookbackDays
	}
	return s.SinceDays
}

func IMessageWindow(s memory.Source, now time.Time) memory.FetchWindow {
	days := IMessageLookbackDays(s)
	if days < 0 {
		return memory.FetchWindow{} // all-time (Since zero ⇒ no lower bound)
	}
	return memory.FetchWindow{Since: now.AddDate(0, 0, -days)}
}

func AppleCalendarWindow(s memory.Source, now time.Time) memory.FetchWindow {
	days := s.SinceDays
	switch {
	case days < 0:
		return memory.FetchWindow{Until: now.AddDate(0, 0, 180)}
	case days == 0:
		days = 90
	}
	return memory.FetchWindow{Since: now.AddDate(0, 0, -days), Until: now.AddDate(0, 0, 180)}
}

func DefaultFilesystemSourceName(path string) string {
	base := filepath.Base(filepath.Clean(path))
	if base == "." || base == string(filepath.Separator) || base == "" {
		return "filesystem"
	}
	return base
}

func CuratedAllowedExt(ext string) bool {
	switch strings.ToLower(ext) {
	case ".md", ".markdown", ".txt", ".text", ".rst", ".json", ".yaml", ".yml", ".toml", ".csv":
		return true
	default:
		return false
	}
}

func CuratedExtractExt(ext string) bool {
	switch strings.ToLower(ext) {
	case ".docx", ".pdf":
		return true
	default:
		return false
	}
}

func CuratedMetadataFile(name string) bool {
	switch name {
	case "go.mod", "go.sum", "Makefile", "Dockerfile", "CLAUDE.md", "AGENTS.md",
		"README", "package.json", "pyproject.toml", "requirements.txt", "Cargo.toml", "CHANGELOG.md":
		return true
	}
	return false
}
