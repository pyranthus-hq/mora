package index

import (
	"context"
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

// ClearAttemptFailure removes the failure stamp after a successful rebuild has
// committed and retired its markers and journals. It removes only a failure
// stamped at or before coveredThrough (the covering rebuild's listing instant),
// so a concurrent attempt that failed later keeps its diagnosis.
func ClearAttemptFailure(ctx context.Context, db *sql.DB, coveredThrough time.Time) error {
	_, err := db.ExecContext(ctx,
		`DELETE FROM index_meta WHERE key IN ('index_last_error','index_last_attempt_at')
		 AND COALESCE((SELECT value FROM index_meta WHERE key='index_last_attempt_at'), '') <= ?`,
		coveredThrough.UTC().Format(time.RFC3339))
	return err
}
