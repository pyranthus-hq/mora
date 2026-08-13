// Package index owns the embedded SQLite search-index storage boundary.
package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/pyranthus-hq/mora/internal/config"
	_ "modernc.org/sqlite"
)

// Path returns the configured index database path.
func Path(cfg config.Config) string { return filepath.Join(cfg.DataDir, "index.db") }

// SchemaMatches reports whether the on-disk user_version matches the binary.
func SchemaMatches(ctx context.Context, cfg config.Config, expectedVersion int) (bool, error) {
	db, err := sql.Open("sqlite", ReadOnlyDSN(cfg))
	if err != nil {
		return false, err
	}
	defer db.Close()
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return false, err
	}
	return version == expectedVersion, nil
}

// ReadOnlyDSN is the DSN every read-only index open uses. Writers persist WAL mode;
// readers only need to use that committed mode, not set it again. A hierarchical
// file URI makes modernc enforce mode=ro instead of silently opening the bare-path
// spelling read-write. When no live WAL exists, immutable=1 is safe for this
// per-request snapshot and lets sandboxed agents read index.db without permission
// to create WAL/SHM sidecars. A live WAL keeps ordinary mode=ro so its committed
// rows remain visible. query_only is a second fail-closed guard; busy_timeout still
// covers a live writer's short lock window.

// ReadOnlyDSN returns the fail-closed live-reader SQLite URI.
func ReadOnlyDSN(cfg config.Config) string {
	path := filepath.ToSlash(Path(cfg))
	if filepath.VolumeName(path) != "" && !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	u := &url.URL{Scheme: "file", Path: path}
	dsn := u.String() + "?mode=ro&_pragma=busy_timeout(15000)&_pragma=query_only(1)"
	// immutable=1 must never be selected merely because the WAL is absent at
	// this instant: an incremental writer can create it immediately after this
	// check, and an immutable reader may then ignore committed pages or observe a
	// malformed image. Use it only for an actually non-writable directory, where
	// this process cannot create WAL/SHM sidecars and cannot race its own writer.
	// The normal writable path remains a live mode=ro connection.
	dirInfo, dirErr := os.Stat(filepath.Dir(Path(cfg)))
	dirReadOnly := dirErr == nil && dirInfo.Mode().Perm()&0o222 == 0
	if dirReadOnly {
		if info, err := os.Stat(Path(cfg) + "-wal"); errors.Is(err, os.ErrNotExist) || (err == nil && info.Size() == 0) {
			dsn += "&immutable=1"
		}
	}
	return dsn
}

// ReadWriteDSN is the DSN every WRITER of the live index (full rebuild + incremental
// upsert) uses. Two pragmas carry the concurrency contract:
//   - _txlock=immediate grabs the writer lock at BeginTx (not lazily mid-tx), so
//     two concurrent rebuilds serialize instead of both starting and one hitting an
//     un-retryable SQLITE_BUSY inside an open transaction.
//   - journal_mode(WAL) is what lets N long-lived `mora mcp serve` READER processes
//     coexist with a writer. In the default rollback journal a writer's EXCLUSIVE
//     lock is incompatible with every reader's SHARED lock, so under real
//     multi-process load (each agent session holds one mcp serve) a write waits for
//     ALL readers, blows past busy_timeout, and surfaces "database is locked". In
//     WAL readers and the single writer never block each other. WAL persists in the
//     db header, so the first open of THIS or the RO DSN converts a legacy
//     delete-mode index in place; thereafter the pragma is a no-op. modernc opens
//     even mode=ro connections read-write, so a reader can create the -wal/-shm
//     sidecars — there is no read-only-WAL breakage here.

// ReadWriteDSN returns the serialized WAL-writer SQLite DSN.
func ReadWriteDSN(cfg config.Config) string {
	return Path(cfg) + "?_txlock=immediate&_pragma=busy_timeout(15000)&_pragma=journal_mode(WAL)"
}

// openIndexRO opens the index read-only, refusing to serve a schema this
// binary doesn't understand (a swapped binary otherwise reads missing columns
// or zeroed salience silently). A stale index self-heals inline when
// indexAutoHeal allows; otherwise the error names the exact fix, and
// `mora upgrade` runs the rebuild at the moment the user consented to a slow
// step.

// CheckSchema refuses an index built for another schema version.
func CheckSchema(db *sql.DB, expectedVersion int) error {
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v != expectedVersion {
		return fmt.Errorf("the search index was built by a different mora version (index schema v%d, this binary expects v%d) — run `mora index rebuild`", v, expectedVersion)
	}
	return nil
}

// UpsertSchemaComplete is the physical readiness probe for the
// incremental-write boundary. It deliberately verifies the union of D and E's
// schema changes: the legacy readiness contract already required memories,
// memories_fts, and index_meta; v5 adds D's three memories columns and E's
// three segment tables. Version fencing still protects ordinary read opens.

// UpsertSchemaComplete verifies every table and column required by incremental writes.
func UpsertSchemaComplete(ctx context.Context, db *sql.DB) (bool, error) {
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN
		 ('memories','memories_fts','index_meta','gmail_segments','gmail_segments_fts','gmail_segment_diagnostics')`).Scan(&n); err != nil {
		return false, err
	}
	if n != 6 {
		return false, nil
	}
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(memories)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, typ string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &primaryKey); err != nil {
			return false, err
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return columns["provider"] && columns["account"] && columns["created_at_unix"], nil
}
