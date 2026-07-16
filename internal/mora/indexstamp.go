package mora

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// indexstamp.go — the index_meta rows Gate 2 adds INSIDE the rebuild's committing
// transaction (so "indexed_at advances only on a committed transaction" is true by
// construction) plus the out-of-band content manifest (B1a). No schema bump: these
// are rows in the existing index_meta key/value table, already excluded from the
// rebuild DELETE list.

// indexManifestAlgo IS the manifest format version — a schema bump is forbidden
// (indexSchemaVersion=2 is shared with every subscriber's share index), so the
// format is versioned by this key's value instead. sha256 over "<64-hex>  <relpath>"
// lines, full sha256 (not the 64-bit ContentHash) because this is an integrity
// manifest, keyed by a slash-normalized relative path so the digest is stable
// across a vault move and on Windows.
const indexManifestAlgo = "sha256-relpath-v1"

// manifestLine renders one file's manifest entry: its content sha256 and its
// vault-relative, slash-normalized path.
func manifestLine(cfg Config, path string, sum [32]byte) string {
	rel, err := filepath.Rel(cfg.VaultDir, path)
	if err != nil {
		rel = path
	}
	return hex.EncodeToString(sum[:]) + "  " + filepath.ToSlash(rel)
}

// manifestDigestOf hashes the sorted manifest lines into the committed vault
// content identity. Sorting makes it independent of listing order.
func manifestDigestOf(lines []string) string {
	sort.Strings(lines)
	h := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(h[:])
}

// memoryPaths extracts the .Path of each parsed memory — the paths a rebuild
// demonstrably covered, for the pending-op clearing pass (A3 rule b).
func memoryPaths(mems []Memory) []string {
	out := make([]string, 0, len(mems))
	for _, m := range mems {
		out = append(out, m.Path)
	}
	return out
}

// stampIndexAttemptFailure records last_error/last_attempt_at in a SEPARATE
// committed transaction after a failed rebuild — the failure names WHY without
// masking the original error (the writeBlockRecord pattern). Best-effort and
// idempotent-safe: dirtiness is proven by the ABSENCE of a pending-op clear, never
// by the presence of this string, so if this write also fails the pending op still
// holds the line. A cold-start failure (no index_meta table yet) simply no-ops.
func stampIndexAttemptFailure(cfg Config, cause error) {
	db, err := sql.Open("sqlite", rwIndexDSN(cfg))
	if err != nil {
		return
	}
	defer db.Close()
	now := indexClock().UTC().Format(time.RFC3339)
	_, _ = db.Exec(
		`INSERT INTO index_meta(key,value) VALUES('index_last_attempt_at',?),('index_last_error',?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		now, sanitizeHealthError(cause.Error()))
}
