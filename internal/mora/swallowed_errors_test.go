package mora

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pyranthus-hq/mora/internal/memory"
)

// This file pins the Phase-0 "swallowed errors" fixes (architecture review
// 2026-06-11): rebuildIndex failures on the MCP write/delete path, SaveStatus
// failures on the sync paths, and allMemoryFiles walk errors were all
// discarded — the tool reported success while serving a stale index, losing
// sync health, or silently indexing a subset of the vault.

// poisonInserts installs a trigger that fails EVERY memories insert, forcing
// the next rebuildIndex to fail deterministically (same technique as
// TestRebuildIndexIsAtomic).
func poisonInserts(t *testing.T, cfg Config) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER fail_all BEFORE INSERT ON memories
		BEGIN SELECT RAISE(FAIL, 'forced rebuild failure'); END;`); err != nil {
		t.Fatal(err)
	}
}

// TestMCPWriteMemoryDegradesOnRebuildFailure: write_memory persists the memory
// (vault is truth) and must SURFACE a failed index rebuild — but as a
// degraded SUCCESS, not an error. An isError result for a write that
// succeeded invites the MCP client to retry, and every retry mints a new
// server-side ID: N retries = N duplicate memories in the vault. The result
// carries the saved memory, an index_stale flag, and a warning naming the
// recovery step.
func TestMCPWriteMemoryDegradesOnRebuildFailure(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	poisonInserts(t, cfg)

	res, err := callMCPTool(context.Background(), "write_memory", map[string]any{
		"title": "Poisoned", "text": "the index rebuild after this write fails",
	})
	if err != nil {
		t.Fatalf("write_memory must not signal failure for a write that SUCCEEDED (an isError result invites a duplicate-minting retry): %v", err)
	}
	degraded, ok := res.(map[string]any)
	if !ok {
		t.Fatalf("degraded write should return a structured result, got %T", res)
	}
	if stale, _ := degraded["index_stale"].(bool); !stale {
		t.Fatalf("degraded result must carry index_stale=true, got %+v", degraded)
	}
	warning, _ := degraded["warning"].(string)
	if !strings.Contains(warning, "mora index rebuild") {
		t.Fatalf("warning must name the recovery step (`mora index rebuild`), got %q", warning)
	}
	if _, ok := degraded["memory"].(Memory); !ok {
		t.Fatalf("degraded result must carry the saved memory (with its ID) so the client never re-writes, got %+v", degraded)
	}
	// The memory itself must be on disk — the vault write succeeded and the
	// index is a disposable derived cache.
	files, ferr := allMemoryFiles(cfg)
	if ferr != nil {
		t.Fatal(ferr)
	}
	if len(files) != 1 {
		t.Fatalf("expected the written memory on disk despite the rebuild failure, found %d files", len(files))
	}
}

// TestMCPDeleteMemorySurfacesRebuildFailure: delete_memory removes the file
// but must surface a failed rebuild — otherwise search keeps serving the
// deleted content as if it still existed.
func TestMCPDeleteMemorySurfacesRebuildFailure(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	res, err := callMCPTool(context.Background(), "write_memory", map[string]any{
		"title": "Doomed", "text": "this memory is deleted under a poisoned index",
	})
	if err != nil {
		t.Fatal(err)
	}
	doomed, ok := res.(Memory)
	if !ok {
		t.Fatalf("write_memory returned %T, want Memory", res)
	}
	// A second memory keeps the failing rebuild non-empty (the trigger fires on
	// INSERT, so an empty vault would rebuild "successfully").
	if _, err := callMCPTool(context.Background(), "write_memory", map[string]any{
		"title": "Survivor", "text": "stays in the vault",
	}); err != nil {
		t.Fatal(err)
	}

	poisonInserts(t, cfg)
	_, err = callMCPTool(context.Background(), "delete_memory", map[string]any{"id": doomed.ID})
	if err == nil {
		t.Fatal("delete_memory returned success despite a failed index rebuild — search would keep serving the deleted memory")
	}
	if !strings.Contains(err.Error(), "mora index rebuild") {
		t.Fatalf("error must tell the agent/user the recovery step (`mora index rebuild`), got: %v", err)
	}
	if _, ferr := os.Stat(memoryPath(cfg, doomed)); !os.IsNotExist(ferr) {
		t.Fatalf("the file deletion itself succeeded and must stick (stat err: %v)", ferr)
	}
}

// TestPersistSyncStatusReportsFailure pins the shared helper the four sync
// paths route through: a failed SaveStatus is warned AND returned (it corrupts
// the three-state digest health), while a sync error stays the primary error.
func TestPersistSyncStatusReportsFailure(t *testing.T) {
	dir := t.TempDir()
	// Parent of the status path is a FILE, so SaveStatus cannot create it.
	if err := os.WriteFile(filepath.Join(dir, "blocked"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	blockedPath := filepath.Join(dir, "blocked", "status.json")
	st := &memory.SyncStatus{Source: "gmail"}

	var buf bytes.Buffer
	err := persistSyncStatus(&buf, blockedPath, st, nil)
	if err == nil {
		t.Fatal("SaveStatus failure was swallowed; sync health silently lost")
	}
	if !strings.Contains(buf.String(), "sync status") {
		t.Fatalf("expected a warning naming the sync status, got %q", buf.String())
	}

	// A sync error stays primary — the save failure must not mask it.
	syncErr := errors.New("fetch exploded")
	if got := persistSyncStatus(&buf, blockedPath, st, syncErr); !errors.Is(got, syncErr) {
		t.Fatalf("sync error must stay the returned error, got %v", got)
	}

	// Healthy path: a writable location with no sync error returns nil.
	okPath := filepath.Join(dir, "sync", "status.json")
	if err := persistSyncStatus(&buf, okPath, st, nil); err != nil {
		t.Fatalf("healthy persist returned %v", err)
	}
	// nil out (no progress writer) must not panic on failure.
	if err := persistSyncStatus(nil, blockedPath, st, nil); err == nil {
		t.Fatal("nil-writer persist failure swallowed")
	}
}

// TestAllMemoryFilesSurfacesWalkErrors: an unreadable directory inside the
// vault must fail the walk, not silently shrink the index to the readable
// subset (a rebuild would then "succeed" with memories missing).
func TestAllMemoryFilesSurfacesWalkErrors(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; chmod 000 does not block reads")
	}
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	locked := filepath.Join(memoriesRoot(cfg), "locked")
	if err := os.MkdirAll(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "hidden.md"), []byte("---\nid: hidden\n---\nx\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })

	if _, err := allMemoryFiles(cfg); err == nil {
		t.Fatal("unreadable vault directory was silently skipped; the walk must surface the error")
	}
}

// TestAllMemoryFilesMissingRootsAreFine pins the non-error case the fix must
// preserve: a fresh vault without a sources/ tree (or any tree at all) is not
// an error — it is an empty vault.
func TestAllMemoryFilesMissingRootsAreFine(t *testing.T) {
	withTempHome(t)
	cfg := mustConfig(t) // no init: neither memories/ nor sources/ exists
	files, err := allMemoryFiles(cfg)
	if err != nil {
		t.Fatalf("missing roots must read as an empty vault, got error: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("expected no files, got %d", len(files))
	}
}

// TestNamedSourceIngestRebuildsDespitePartialFailure: with the partial-failure
// contract (Ingest returns non-nil after dropping items), the named-source
// path (`mora ingest run --source X`) must still rebuild the index before
// surfacing the error — the successfully written items are already in the
// vault, and skipping the rebuild leaves them unsearchable with no auto-heal
// (review finding: the early return predated the contract change).
func TestNamedSourceIngestRebuildsDespitePartialFailure(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	// A memory in the vault that is NOT yet in the index (writeMemory does not
	// rebuild): the named ingest's final rebuild is what makes it searchable.
	if err := writeMemory(cfg, Memory{ID: "fresh1", Scope: "global", Type: "insight", Title: "Fresh", Text: "alphapartialtoken"}); err != nil {
		t.Fatal(err)
	}
	if err := saveSources(cfg, []Source{{Name: "docs", Type: "filesystem", Path: t.TempDir(), Scope: "global", Enabled: ptr(true), CreatedAt: "2026-06-11T00:00:00Z"}}); err != nil {
		t.Fatal(err)
	}
	prev := ingestSourceFn
	ingestSourceFn = func(cfg Config, s Source, out io.Writer) (int, error) {
		return 3, errors.New("2 item(s) failed to write and were dropped")
	}
	t.Cleanup(func() { ingestSourceFn = prev })

	var out bytes.Buffer
	err := Run(context.Background(), []string{"ingest", "run", "--source", "docs"}, &out, &out, strings.NewReader(""))
	if err == nil {
		t.Fatal("named-source partial failure must surface as an error")
	}

	// The rebuild must still have run: fresh1 is in index.db. Query the index
	// directly — searchMemories would mask the gap by rebuilding on a missing
	// db, and the schema auto-heal only fires on version mismatch, not rows.
	db, derr := sql.Open("sqlite", roIndexDSN(cfg))
	if derr != nil {
		t.Fatal(derr)
	}
	defer db.Close()
	var n int
	if qerr := db.QueryRow(`SELECT count(*) FROM memories WHERE id = 'fresh1'`).Scan(&n); qerr != nil {
		t.Fatal(qerr)
	}
	if n != 1 {
		t.Fatalf("index was not rebuilt after the partial named-source run: fresh1 rows = %d, want 1 (written memories left unsearchable)", n)
	}
}
