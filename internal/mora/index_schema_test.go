package mora

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// rebuildIndex must stamp the schema version it writes (PRAGMA user_version) —
// the stamp is what lets read paths detect an index left behind by a different
// binary instead of serving it silently.
func TestRebuildIndexStampsSchemaVersion(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	if err := writeMemory(cfg, Memory{ID: newID(), Scope: "global", Type: "note", Title: "t", Text: "hello world", CreatedAt: "2026-06-01T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath(cfg)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != indexSchemaVersion {
		t.Fatalf("rebuild stamped user_version=%d, want %d", v, indexSchemaVersion)
	}
}

// An index whose schema doesn't match the binary must fail LOUDLY with the
// exact fix — not degrade silently (the live Phase-14 failure mode: a swapped
// binary served zeroed salience off a pre-column index). Pre-stamp indexes
// read as user_version 0, so forcing 0 simulates every legacy vault.
func TestStaleIndexSchemaErrorsActionably(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	if err := writeMemory(cfg, Memory{ID: newID(), Scope: "global", Type: "note", Title: "t", Text: "hello world", CreatedAt: "2026-06-01T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 0`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()

	if _, err := openIndexRO(cfg); err == nil || !strings.Contains(err.Error(), "mora index rebuild") {
		t.Fatalf("openIndexRO on a stale index: want an actionable error naming `mora index rebuild`, got: %v", err)
	}
	if _, err := searchMemories(context.Background(), cfg, "hello", "", 5); err == nil || !strings.Contains(err.Error(), "mora index rebuild") {
		t.Fatalf("searchMemories must surface the schema error, got: %v", err)
	}
	// And a rebuild heals it — the error message's own advice must work.
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := searchMemories(context.Background(), cfg, "hello", "", 5); err != nil {
		t.Fatalf("search should work again after the advised rebuild: %v", err)
	}
}

// postUpgradeRebuild must exec the swapped-in binary (`<exe> index rebuild`) —
// the running process is still the OLD code; schema knowledge lives in the new
// executable. A failing child surfaces an error (warn-don't-fail is the
// caller's policy, the swap itself already succeeded).
func TestPostUpgradeRebuildExecsNewBinary(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-mora")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho fake rebuild: \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := postUpgradeRebuild(context.Background(), script, &out); err != nil {
		t.Fatalf("post-upgrade rebuild via fake binary failed: %v", err)
	}
	if !strings.Contains(out.String(), "fake rebuild: index rebuild") {
		t.Fatalf("new binary not invoked as `index rebuild`: %q", out.String())
	}

	bad := filepath.Join(dir, "fake-bad")
	if err := os.WriteFile(bad, []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := postUpgradeRebuild(context.Background(), bad, &out); err == nil {
		t.Fatal("a failing rebuild must surface an error to the caller")
	}
}
