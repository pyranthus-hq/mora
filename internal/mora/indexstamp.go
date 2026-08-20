package mora

import (
	indexstore "github.com/pyranthus-hq/mora/internal/index"
)

// indexstamp.go — the index_meta rows Gate 2 adds INSIDE the rebuild's committing
// transaction (so "indexed_at advances only on a committed transaction" is true by
// construction) plus the out-of-band content manifest (B1a). No schema bump: these
// are rows in the existing index_meta key/value table, already excluded from the
// rebuild DELETE list.

// indexManifestAlgo IS the manifest format version. The DB schema stamp is shared
// with every subscriber's share index, so this independently evolving manifest
// format is versioned by the key's value instead. sha256 over "<64-hex>  <relpath>"
// lines, full sha256 (not the 64-bit ContentHash) because this is an integrity
// manifest, keyed by a slash-normalized relative path so the digest is stable
// across a vault move and on Windows.
const indexManifestAlgo = indexstore.ManifestAlgorithm

// The composition root retains narrow adapters for rebuild call sites.
func manifestLine(cfg Config, path string, sum [32]byte) string {
	return indexstore.ManifestLine(cfg, path, sum)
}
func manifestDigestOf(lines []string) string { return indexstore.ManifestDigest(lines) }
func stampIndexAttemptFailure(cfg Config, cause error) {
	indexstore.StampAttemptFailure(cfg, cause, indexClock(), sanitizeHealthError)
}

// memoryPaths extracts parsed paths covered by a rebuild.
func memoryPaths(mems []Memory) []string {
	out := make([]string, 0, len(mems))
	for _, m := range mems {
		out = append(out, m.Path)
	}
	return out
}
