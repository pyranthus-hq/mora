// Package genericutil holds small, unrelated pure helpers with zero
// dependency on any other internal package: a bool-pointer literal, TTY
// detection, rune-safe truncation, a file-existence check, CSV splitting,
// home-directory expansion, and a help-flag check.
package genericutil

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Ptr returns a pointer to b. Handy for setting an *bool struct field from a
// literal, where leaving the field nil would carry different zero-value
// semantics than an explicit false.
func Ptr(b bool) *bool { return &b }

// IsInteractive reports whether r is a real terminal (character device). It
// uses only the stdlib: in production stdin is *os.File (os.Stdin) and this
// checks for ModeCharDevice; in tests/pipes stdin is a strings.Reader or a
// redirected file, so this returns false. This keeps interactive
// consent/menu prompts from blocking on a non-TTY without adding a
// go-isatty dependency.
func IsInteractive(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// TruncateRunes returns s clipped to at most max bytes, never splitting a
// multi-byte UTF-8 rune (a raw s[:max] could leave an invalid trailing byte).
func TruncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// FileExists reports whether p exists (any stat error, including permission
// denied, counts as not-existing).
func FileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// SplitCSV splits s on commas, trims whitespace from each field, and drops
// empty fields.
func SplitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ExpandHome expands a leading "~/" in p to the current user's home
// directory. Any other form (bare "~", "~user", or a path without a leading
// "~/") is returned unchanged.
func ExpandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[2:])
	}
	return p
}

// IsHelpFlag reports whether a subcommand arg is a help request. Used by
// subcommands that otherwise treat a leading flag as data and act on it.
func IsHelpFlag(s string) bool {
	return s == "--help" || s == "-h" || s == "help"
}
