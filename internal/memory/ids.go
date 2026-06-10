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
