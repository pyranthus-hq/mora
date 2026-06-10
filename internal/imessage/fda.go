package imessage

import (
	"database/sql"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (no CGO)
)

// chatDBDSN builds the read-only DSN for opening chat.db.
//
// mode=ro: read-only; SQLite applies the WAL sidecars on read (SQLite 3.22+) so a
// live Messages.app's uncheckpointed messages are still visible. NEVER immutable=1
// (that would ignore the WAL → stale/torn reads, dropping recent messages, IMSG-09).
// busy_timeout: tolerate a transient lock while Messages.app checkpoints.
func chatDBDSN(path string) string {
	return "file:" + path + "?mode=ro&_pragma=busy_timeout(5000)"
}

// openChatDB opens chat.db read-only and forces a real read so a permission-denied
// open surfaces immediately. sql.Open is lazy; macOS Full-Disk-Access denial lets
// os.Stat succeed while open() fails — so we Ping() to force the actual open/read
// (the doctor readability signal). This is the FDA probe; it never mutates the DB.
func openChatDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", chatDBDSN(path))
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// ProbeReadable reports whether chat.db can actually be opened and a real row read
// — the readability signal mora doctor uses for Full Disk Access (IMSG-08/09). It
// returns false (with the underlying error) when FDA is denied: os.Stat would
// succeed but the open/read fails. Shared with the connector's own open path so the
// doctor and the live fetcher agree on what "readable" means.
func ProbeReadable(path string) (bool, error) {
	db, err := openChatDB(path)
	if err != nil {
		return false, err
	}
	defer db.Close()
	// Force a real read beyond the open: query sqlite_master so an FDA-denied or
	// corrupt DB surfaces here rather than at first FetchPage.
	var n int
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master").Scan(&n); err != nil {
		return false, err
	}
	return true, nil
}
