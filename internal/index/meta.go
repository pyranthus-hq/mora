package index

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/config"
)

// ManifestAlgorithm versions the deterministic vault content-manifest format.
const ManifestAlgorithm = "sha256-relpath-v1"

// ManifestLine renders one file's vault-relative, slash-normalized digest entry.
func ManifestLine(cfg config.Config, path string, sum [32]byte) string {
	rel, err := filepath.Rel(cfg.VaultDir, path)
	if err != nil {
		rel = path
	}
	return hex.EncodeToString(sum[:]) + "  " + filepath.ToSlash(rel)
}

// ManifestDigest hashes deterministically sorted manifest lines.
func ManifestDigest(lines []string) string {
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}

// StampAttemptFailure best-effort records why an index rebuild failed.
func StampAttemptFailure(cfg config.Config, cause error, now time.Time, sanitize func(string) string) {
	db, err := sql.Open("sqlite", ReadWriteDSN(cfg))
	if err != nil {
		return
	}
	defer db.Close()
	_, _ = db.Exec(`INSERT INTO index_meta(key,value) VALUES('index_last_attempt_at',?),('index_last_error',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, now.UTC().Format(time.RFC3339), sanitize(cause.Error()))
}
