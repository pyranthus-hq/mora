package index

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pyranthus-hq/mora/internal/config"
)

func TestCheckSchemaMatchAndMismatch(t *testing.T) {
	const version = 5
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "schema.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec("PRAGMA user_version = 5"); err != nil {
		t.Fatal(err)
	}
	if err = CheckSchema(db, version); err != nil {
		t.Fatalf("matching version: %v", err)
	}
	if _, err = db.Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}
	err = CheckSchema(db, version)
	if err == nil {
		t.Fatal("wrong version must error")
	}
	for _, want := range []string{"different mora version", "index schema v99", "expects v5", "mora index rebuild"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("schema error %q missing %q", err, want)
		}
	}
	closed, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "closed.db"))
	if err != nil {
		t.Fatal(err)
	}
	_ = closed.Close()
	if err = CheckSchema(closed, version); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("closed db error=%v", err)
	}
}

func TestDSNsAndSchemaProbes(t *testing.T) {
	cfg := config.Config{DataDir: t.TempDir()}
	if got := Path(cfg); got != filepath.Join(cfg.DataDir, "index.db") {
		t.Fatalf("Path=%q", got)
	}
	rw := ReadWriteDSN(cfg)
	if strings.Contains(rw, "immutable") || !strings.Contains(rw, "journal_mode(WAL)") {
		t.Fatalf("writer DSN=%q", rw)
	}
	ro := ReadOnlyDSN(cfg)
	if !strings.HasPrefix(ro, "file:") || !strings.Contains(ro, "mode=ro") || !strings.Contains(ro, "query_only(1)") {
		t.Fatalf("reader DSN=%q", ro)
	}
	db, err := sql.Open("sqlite", rw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("PRAGMA user_version=5"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	ok, err := SchemaMatches(context.Background(), cfg, 5)
	if err != nil || !ok {
		t.Fatalf("SchemaMatches=(%v,%v)", ok, err)
	}
	ok, err = SchemaMatches(context.Background(), cfg, 6)
	if err != nil || ok {
		t.Fatalf("mismatch=(%v,%v)", ok, err)
	}
}

func TestReadOnlyDSNUsesImmutableOnlyForReadOnlyDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses mode bits")
	}
	dir := t.TempDir()
	cfg := config.Config{DataDir: dir}
	if err := os.WriteFile(Path(cfg), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if got := ReadOnlyDSN(cfg); !strings.Contains(got, "immutable=1") {
		t.Fatalf("read-only directory DSN=%q", got)
	}
}
