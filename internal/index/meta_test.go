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
