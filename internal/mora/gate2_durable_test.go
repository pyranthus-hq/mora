package mora

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pyranthus-hq/mora/internal/memory"
)

// gate2_durable_test.go — the durable-marker call-trace gates (matrix rows 33/33b),
// the crash-window acceptance test, and the ingest-journal recovery gates (rows
// 34a/34b). fsync is unobservable from userspace (a process crash preserves
// page-cache data), so the durability rows are CALL-TRACE assertions through the
// markerSyncFn/syncDirFn seams; the MUTATION is deleting the production call in
// atomicWriteDurable's body, not flipping the seam.

// recordingSync wraps the real barriers and appends an event to a shared trace so a
// test can assert the ORDER of fsync / rename / dir-sync.
func withMarkerTrace(t *testing.T) *[]string {
	t.Helper()
	trace := &[]string{}
	origMarker, origDir := markerSyncFn, syncDirFn
	markerSyncFn = func(f *os.File) error { *trace = append(*trace, "fsync"); return origMarker(f) }
	syncDirFn = func(dir string) error { *trace = append(*trace, "dirsync"); return origDir(dir) }
	t.Cleanup(func() { markerSyncFn, syncDirFn = origMarker, origDir })
	return trace
}

// TestDurableMarkerFsyncsBeforeRename (matrix row 33) — the temp file's f.Sync fires
// BEFORE the rename. MUTATION: delete the markerSyncFn(f) call in atomicWriteDurable
// => no "fsync" event => RED.
func TestDurableMarkerFsyncsBeforeRename(t *testing.T) {
	dir := t.TempDir()
	trace := withMarkerTrace(t)
	if err := atomicWriteDurable(filepath.Join(dir, "marker.json"), []byte(`{"x":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(*trace, ",")
	if !strings.Contains(got, "fsync") {
		t.Fatalf("no data fsync in the durable-write trace %q", got)
	}
	// fsync must be the FIRST event (before the rename, which precedes dirsync).
	if (*trace)[0] != "fsync" {
		t.Fatalf("trace = %q, want fsync first (data synced before the rename)", got)
	}
}

// TestDurableMarkerSyncsDirBeforeReturn (matrix row 33b) — the parent-dir sync fires
// before atomicWriteDurable returns (i.e. before any vault publish may begin).
// MUTATION: delete the syncDirFn(dir) call => no "dirsync" event => RED.
func TestDurableMarkerSyncsDirBeforeReturn(t *testing.T) {
	dir := t.TempDir()
	trace := withMarkerTrace(t)
	if err := atomicWriteDurable(filepath.Join(dir, "marker.json"), []byte(`{"x":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(*trace, ",")
	if !strings.Contains(got, "dirsync") {
		t.Fatalf("no parent-dir sync in the durable-write trace %q", got)
	}
	// dirsync is the LAST barrier, and it must fire before return: fsync before dirsync.
	if (*trace)[len(*trace)-1] != "dirsync" {
		t.Fatalf("trace = %q, want dirsync last (dir synced before return)", got)
	}
}

// TestMarkerSurvivesCrashBeforeVaultPublish (acceptance) — a crash between the
// marker's durable write and the vault publish leaves the marker present and the
// index dirty, never false-clean. The testHookPostMarkerWrite seam simulates the
// crash in exactly the mark-before-visible window.
func TestMarkerSurvivesCrashBeforeVaultPublish(t *testing.T) {
	cfg := gate2Vault(t)
	ctx := context.Background()

	// A write marks its op, then "crashes" (panic caught) before the vault publish.
	target := filepath.Join(memoriesRoot(cfg), "global", "crashy.md")
	crashed := false
	func() {
		defer func() { _ = recover(); crashed = true }()
		testHookPostMarkerWrite = func() { panic("simulated crash after durable marker, before publish") }
		defer func() { testHookPostMarkerWrite = nil }()
		_, _ = markIndexDirty(ctx, cfg, pendingOp{Kind: opKindWrite, Path: target})
	}()
	if !crashed {
		t.Fatal("crash simulation did not fire")
	}
	// The marker is on stable storage; the index reads dirty and recovers next rebuild.
	if st := gate2IndexState(t, cfg); st != idxDirty {
		t.Fatalf("index state after crash-before-publish = %q, want dirty (marker survived)", st)
	}
}

// TestKilledIngestRecovers (matrix rows 34a/34b) — a killed connector backfill
// leaves a durable journal so the index reads dirty and the next rebuild recovers
// every published path; the header's durability is the load-bearing barrier.
func TestKilledIngestRecovers(t *testing.T) {
	t.Run("journal_before_first_file", func(t *testing.T) {
		// MUTATION (row 34a): remove the production ensureIngestJournalHeader call from
		// writeMappedMemory (journal AFTER the terminal rebuild instead of BEFORE the
		// first publish). Then a SIGKILL after a publish but before the rebuild leaves
		// NO journal => the recovery rebuild is clean-and-missing => this test's "dirty
		// after the killed publish" assertion goes RED. This drives the REAL
		// writeMappedMemory (the mutated site), NOT a hand-built header, so the mutation
		// is load-bearing.
		cfg := gate2Vault(t)
		mm := memory.MappedMemory{
			StableID: "gmail_thread_x", Scope: "global", Type: "email", Title: "Killed",
			Body: "killedbody", Source: "gmail", Provider: "gmail",
			CreatedAt: nowRFC3339(), ContentHash: "hash_killed",
		}
		// Production publish path, SIGKILLed in the publish->journal-line window: the
		// file lands, but the best-effort path line never appends. Only the durable
		// header (written BEFORE the publish) keeps the index dirty — remove that
		// production call and this crash is a clean-and-missing false-clean.
		crashed := false
		func() {
			defer func() { _ = recover(); crashed = true }()
			testHookPostConnectorPublish = func() { panic("SIGKILL after publish, before journal line") }
			defer func() { testHookPostConnectorPublish = nil }()
			_ = writeMappedMemory(cfg, mm)
		}()
		if !crashed {
			t.Fatal("crash simulation did not fire")
		}
		sourceKey := ingestSourceKey(mm.Provider, mm.Account)
		published := filepath.Join(sourcesRoot(cfg), "gmail", "gmail_thread_x.md")
		if _, err := os.Stat(published); err != nil {
			t.Fatalf("connector memory did not land: %v", err)
		}
		// The ingesting process is now dead: a real kill leaves a lease naming a
		// now-dead pid that the next rebuild reclaims. Model that by dropping this
		// (still-live) test process's lease.
		_ = os.Remove(ingestLeasePath(cfg, sourceKey))

		// A killed ingest never truncated its journal => the index reads dirty.
		if st := gate2IndexState(t, cfg); st != idxDirty {
			t.Fatalf("index state after killed ingest = %q, want dirty", st)
		}
		// The next committed rebuild lists the published file, indexes it, and
		// retires the journal — never clean-and-missing.
		if _, err := rebuildIndex(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
		if st := gate2IndexState(t, cfg); st != idxFresh {
			t.Fatalf("index state after recovery rebuild = %q, want fresh", st)
		}
		if dirty, _, _, _ := ingestJournalStatus(cfg); dirty {
			t.Fatal("ingest journal survived a covering rebuild")
		}
		if res := gate2Search(t, cfg, "killedbody"); len(res) != 1 {
			t.Fatalf("recovered memory not searchable: %+v", res)
		}
	})

	t.Run("journal_header_synced", func(t *testing.T) {
		// The header write goes through the durable barriers (fsync + dir sync).
		cfg := gate2Vault(t)
		trace := withMarkerTrace(t)
		if err := ensureIngestJournalHeader(cfg, ingestSourceKey("gmail", "")); err != nil {
			t.Fatal(err)
		}
		got := strings.Join(*trace, ",")
		if !strings.Contains(got, "fsync") || !strings.Contains(got, "dirsync") {
			t.Fatalf("ingest journal header trace = %q, want both fsync and dirsync (durable header)", got)
		}
	})
}
