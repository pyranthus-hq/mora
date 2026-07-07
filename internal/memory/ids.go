package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// StableID is derived from immutable provider identity only — never content —
// so re-syncing an edited item overwrites the same file instead of duplicating.
func StableID(kind ItemKind, providerID string) string {
	return string(kind) + "/" + providerID
}

// ContentHash drives change detection (stored separately from the ID).
func ContentHash(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:])[:16]
}

// SafeFilename turns a StableID into a filesystem-safe base name.
func SafeFilename(stableID string) string {
	r := strings.NewReplacer("/", "_", ":", "_", " ", "_")
	return r.Replace(stableID)
}

// SanitizeWindowsBase maps s to a base filename that is legal on Windows. It is
// a pure function with no GOOS check so it can be unit-tested on any platform;
// callers gate its use on runtime.GOOS (see osSafeBase in the mora package).
//
// Unlike SafeFilename (which only maps / : and space), this covers the full
// reserved set Windows rejects — the control range 0x00–0x1F and the characters
// < > : " / \ | ? * each become '_'. Trailing dots and spaces are trimmed
// because Windows silently strips them, which would otherwise desync a write
// path from the lookup that reconstructs the name. A reserved DOS device name
// (CON, PRN, AUX, NUL, COM1–9, LPT1–9 — case-insensitive, with or without an
// extension) is prefixed with '_' since Windows reserves it as a whole path
// component. An empty or fully-stripped result falls back to "_".
//
// Space is intentionally left untouched: it is legal mid-name on Windows, so a
// title like "Dinner Friday?" maps to "Dinner Friday_", not "Dinner_Friday_".
func SanitizeWindowsBase(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r < 0x20 || strings.ContainsRune(`<>:"/\|?*`, r) {
			b.WriteByte('_')
			continue
		}
		b.WriteRune(r)
	}
	out := strings.TrimRight(b.String(), ". ")
	if out == "" {
		return "_"
	}
	if isWindowsReservedName(out) {
		return "_" + out
	}
	return out
}

// isWindowsReservedName reports whether base collides with a reserved DOS device
// name. The name is reserved regardless of extension ("CON" and "CON.md" both),
// so the check inspects the segment before the first dot.
func isWindowsReservedName(base string) bool {
	name := base
	if i := strings.IndexByte(name, '.'); i >= 0 {
		name = name[:i]
	}
	switch strings.ToUpper(name) {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	up := strings.ToUpper(name)
	if len(up) == 4 && (strings.HasPrefix(up, "COM") || strings.HasPrefix(up, "LPT")) {
		return up[3] >= '1' && up[3] <= '9'
	}
	return false
}
