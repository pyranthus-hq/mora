package index

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/config"
)

func TestManifestDeterminism(t *testing.T) {
	cfg := config.Config{VaultDir: filepath.Join("root", "vault")}
	sum := sha256.Sum256([]byte("body"))
	got := ManifestLine(cfg, filepath.Join(cfg.VaultDir, "memories", "a.md"), sum)
	if !strings.HasSuffix(got, "  memories/a.md") {
		t.Fatalf("ManifestLine=%q", got)
	}
	a := []string{"b", "a"}
	b := []string{"a", "b"}
	if ManifestDigest(a) != ManifestDigest(b) {
		t.Fatal("digest must ignore listing order")
	}
	if a[0] != "a" {
		t.Fatal("ManifestDigest must retain existing in-place sort behavior")
	}
}
func TestStampAttemptFailure(t *testing.T) {
	cfg := config.Config{DataDir: t.TempDir()}
	db, err := sql.Open("sqlite", ReadWriteDSN(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TABLE index_meta(key TEXT PRIMARY KEY,value TEXT)`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	now := time.Date(2026, 8, 13, 1, 2, 3, 0, time.FixedZone("x", 3600))
	StampAttemptFailure(cfg, context.Canceled, now, func(s string) string { return "safe:" + s })
	db, err = sql.Open("sqlite", ReadOnlyDSN(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows := map[string]string{}
	r, err := db.Query(`SELECT key,value FROM index_meta`)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for r.Next() {
		var k, v string
		if err = r.Scan(&k, &v); err != nil {
			t.Fatal(err)
		}
		rows[k] = v
	}
	if rows["index_last_attempt_at"] != "2026-08-13T00:02:03Z" || rows["index_last_error"] != "safe:context canceled" {
		t.Fatalf("rows=%v", rows)
	}
}
func TestStampAttemptFailureColdStartNoop(t *testing.T) {
	cfg := config.Config{DataDir: t.TempDir()}
	StampAttemptFailure(cfg, context.Canceled, time.Time{}, func(s string) string { return s })
}

func TestClearAttemptFailureKeepsLaterFailure(t *testing.T) {
	cfg := config.Config{DataDir: t.TempDir()}
	db, err := sql.Open("sqlite", ReadWriteDSN(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE TABLE index_meta(key TEXT PRIMARY KEY,value TEXT)`); err != nil {
		t.Fatal(err)
	}
	stamp := func(at string) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO index_meta(key,value) VALUES('index_last_attempt_at',?),('index_last_error','database is locked'),('vault_dir','/v')
			ON CONFLICT(key) DO UPDATE SET value=excluded.value`, at); err != nil {
			t.Fatal(err)
		}
	}
	count := func() int {
		t.Helper()
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM index_meta WHERE key IN ('index_last_error','index_last_attempt_at')`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	covered := time.Date(2026, 10, 15, 12, 0, 0, 100_000_000, time.UTC)

	// A failure stamped after the covering rebuild listed belongs to a concurrent
	// attempt this success cannot vouch for: keep it, even within the same second
	// (a whole-second stamp would read as equal and be cleared).
	for _, later := range []string{"2026-10-15T12:00:05Z", "2026-10-15T12:00:00.2Z"} {
		stamp(later)
		if err := ClearAttemptFailure(context.Background(), db, covered); err != nil {
			t.Fatal(err)
		}
		if count() != 2 {
			t.Fatalf("a later failure (%s) was cleared; rows left = %d, want 2", later, count())
		}
	}
	// A failure stamped at or before the listing is superseded by the success,
	// including one earlier in the same second and a legacy whole-second stamp.
	stamp("2026-10-15T12:00:00.05Z")
	if err := ClearAttemptFailure(context.Background(), db, covered); err != nil {
		t.Fatal(err)
	}
	if count() != 0 {
		t.Fatalf("an earlier same-second failure was kept; rows left = %d, want 0", count())
	}
	stamp("2026-10-15T11:59:00Z")
	if err := ClearAttemptFailure(context.Background(), db, covered); err != nil {
		t.Fatal(err)
	}
	if count() != 0 {
		t.Fatalf("superseded failure kept; rows left = %d, want 0", count())
	}
	var vault string
	if err := db.QueryRow(`SELECT value FROM index_meta WHERE key='vault_dir'`).Scan(&vault); err != nil || vault != "/v" {
		t.Fatalf("clear touched provenance rows: vault_dir=%q err=%v", vault, err)
	}
}

func TestStampAttemptFailureKeepsSubSecondPrecision(t *testing.T) {
	cfg := config.Config{DataDir: t.TempDir()}
	db, err := sql.Open("sqlite", ReadWriteDSN(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE TABLE index_meta(key TEXT PRIMARY KEY,value TEXT)`); err != nil {
		t.Fatal(err)
	}
	failedAt := time.Date(2026, 10, 15, 12, 0, 0, 200_000_000, time.UTC)
	StampAttemptFailure(cfg, context.Canceled, failedAt, func(s string) string { return s })
	// A rebuild that listed earlier in the same second must not clear it.
	if err := ClearAttemptFailure(context.Background(), db, failedAt.Add(-100*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM index_meta WHERE key IN ('index_last_error','index_last_attempt_at')`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("same-second later failure cleared: rows=%d err=%v", n, err)
	}
}
