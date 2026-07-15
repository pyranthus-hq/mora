package mora

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/pyranthus-hq/mora/internal/memory"
)

// gate2_review1_test.go — the round-1 adversarial-review remediation gates. Each
// test is RED against the pre-fix HEAD (the finding) and green after the fix.

// idxDeleteAllMeta wipes the index_meta table (a crash mid-rebuild, a hand
// `DELETE FROM index_meta`, or a torn-db restore) by opening the index read-write.
func idxDeleteAllMeta(t *testing.T, cfg Config) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`DELETE FROM index_meta`); err != nil {
		t.Fatalf("delete index_meta: %v", err)
	}
}

// TestWipedIndexMetaFailsClosed (Finding 3) — a schema-valid index whose committed
// provenance rows have been deleted must read FAILED and make doctor --strict exit
// nonzero, never fresh/healthy. Before the fix indexHealthOf accepted an empty
// index_meta map (absent stamps => zero lag => fell through to fresh).
func TestWipedIndexMetaFailsClosed(t *testing.T) {
	cfg := gate2Vault(t)
	if st := gate2IndexState(t, cfg); st != idxFresh {
		t.Fatalf("precondition index state = %q, want fresh", st)
	}
	idxDeleteAllMeta(t, cfg)

	if st := gate2IndexState(t, cfg); st != idxFailed {
		t.Fatalf("wiped-index_meta state = %q, want failed (fail closed)", st)
	}
	var buf bytes.Buffer
	err := cmdDoctor(context.Background(), []string{"--json", "--strict"}, &buf)
	if err == nil {
		t.Fatal("doctor --strict exited 0 on a wiped index_meta")
	}
	var rep doctorReport
	if jerr := json.Unmarshal(buf.Bytes(), &rep); jerr != nil {
		t.Fatal(jerr)
	}
	if rep.Healthy {
		t.Fatal("doctor healthy=true on a wiped index_meta")
	}
	if rep.Index.State != idxFailed {
		t.Fatalf("report index.state = %q, want failed", rep.Index.State)
	}
}

// TestZeroByteJournalFailsClosed (Finding 4) — a present-but-empty ingest journal
// (the crash state appendJournalDurable leaves after creating/opening but before the
// header sync) reads DIRTY and makes doctor exit nonzero, never healthy. Before the
// fix scanJournal mapped a zero-byte file to hasContent=false and it was skipped —
// indistinguishable from an absent journal.
func TestZeroByteJournalFailsClosed(t *testing.T) {
	cfg := gate2Vault(t)
	if st := gate2IndexState(t, cfg); st != idxFresh {
		t.Fatalf("precondition index state = %q, want fresh", st)
	}
	jp := ingestJournalPath(cfg, ingestSourceKey("gmail", ""))
	if err := os.MkdirAll(filepath.Dir(jp), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jp, nil, 0o644); err != nil { // zero bytes
		t.Fatal(err)
	}

	if st := gate2IndexState(t, cfg); st != idxDirty {
		t.Fatalf("zero-byte journal state = %q, want dirty (fail closed)", st)
	}
	var buf bytes.Buffer
	if err := cmdDoctor(context.Background(), []string{"--json", "--strict"}, &buf); err == nil {
		t.Fatal("doctor --strict exited 0 with a zero-byte ingest journal")
	}
	var rep doctorReport
	if jerr := json.Unmarshal(buf.Bytes(), &rep); jerr != nil {
		t.Fatal(jerr)
	}
	if rep.Healthy {
		t.Fatal("doctor healthy=true with a zero-byte ingest journal")
	}
}

// TestConcurrentRebuildKeepsLiveIngestHeader (Finding 2) — a committed rebuild that
// covers a live ingest run's already-published files must NOT retire the run's
// journal header while the run is still publishing (A3 rule d: no retire while a live
// lease is held). Before the fix the rebuild deleted the header unconditionally, so a
// memory published AFTER the rebuild listed — and SIGKILLed before its journal line —
// was a false-clean.
func TestConcurrentRebuildKeepsLiveIngestHeader(t *testing.T) {
	cfg := gate2Vault(t)
	ctx := context.Background()
	sourceKey := ingestSourceKey("gmail", "")

	// Item 1: a real connector publish — writes the durable header, takes the live
	// lease, publishes the file, journals its path.
	mm1 := memory.MappedMemory{
		StableID: "gmail_thread_1", Scope: "global", Type: "email", Title: "One",
		Body: "onebody", Source: "gmail", Provider: "gmail", CreatedAt: nowRFC3339(), ContentHash: "h1",
	}
	if err := writeMappedMemory(cfg, mm1); err != nil {
		t.Fatal(err)
	}
	// Item 2 STARTS its publish: its ensureIngestJournalHeader observes the existing
	// header (a no-op) and keeps the run's lease live — but it has NOT published yet.
	if err := ensureIngestJournalHeader(cfg, sourceKey); err != nil {
		t.Fatal(err)
	}
	// A concurrent committed rebuild covers item 1 while the ingest run is still LIVE.
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	// Item 2 finishes publishing AFTER the rebuild listed, then is SIGKILLed before its
	// journal path line (writeMappedMemory journals only after the publish). Because it
	// already passed its header check, it does NOT re-mark — so the run's header is the
	// only thing keeping the index dirty.
	item2 := filepath.Join(sourcesRoot(cfg), "gmail", "gmail_thread_2.md")
	if err := os.WriteFile(item2, []byte("---\nid: gmail_thread_2\n---\n\ntwobody\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if st := gate2IndexState(t, cfg); st != idxDirty {
		t.Fatalf("index state after a concurrent rebuild during a live ingest = %q, want dirty (item 2 is unindexed)", st)
	}
	if res := gate2Search(t, cfg, "twobody"); len(res) != 0 {
		t.Fatalf("item 2 was published after the rebuild listed; it must NOT be indexed yet: %+v", res)
	}

	// Recovery: the run ends (its lease is released / reclaimed as a dead pid would be)
	// and the next committed rebuild indexes item 2 and retires the journal.
	_ = os.Remove(ingestLeasePath(cfg, sourceKey))
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if st := gate2IndexState(t, cfg); st != idxFresh {
		t.Fatalf("post-recovery index state = %q, want fresh", st)
	}
	if res := gate2Search(t, cfg, "twobody"); len(res) != 1 {
		t.Fatalf("item 2 not recovered after the run ended: %+v", res)
	}
}

// TestGovernanceMutationReindexes (Finding 1) — a governance-ledger command
// (an index input via writeGraph) both marks the index dirty AND runs a covering
// rebuild, so the index is fresh afterward and a FORCED rebuild failure leaves it
// dirty (never a false-clean while the ledger has changed). Before the fix cmdUnforget
// / cmdBriefCorrect returned without marking or rebuilding at all.
func TestGovernanceMutationReindexes(t *testing.T) {
	t.Run("unforget_leaves_index_fresh", func(t *testing.T) {
		cfg := gate2Vault(t)
		seed, err := appendGovernanceEntry(cfg, govEntry{Kind: govKindForget, Action: govActionSuppress, Atom: govAtom{Kind: atomStableID, Value: "gmail_thread_z"}, Reason: "seed"})
		if err != nil {
			t.Fatal(err)
		}
		// A pending marker seeded by an earlier crash must be cleared by unforget's own
		// rebuild — proving the rebuild actually runs.
		if _, err := markIndexDirty(context.Background(), cfg, pendingOp{Kind: opKindRebuild}); err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		if err := cmdUnforget(context.Background(), []string{"--yes", seed.ID}, &buf); err != nil {
			t.Fatal(err)
		}
		if st := gate2IndexState(t, cfg); st != idxFresh {
			t.Fatalf("index state after unforget = %q, want fresh (the command rebuilt)", st)
		}
	})

	t.Run("brief_correct_dirty_when_rebuild_fails", func(t *testing.T) {
		cfg := gate2Vault(t, coreBIdxmem("mem_cite", "global", "insight", "Cited", "citebody"))
		// Force the covering rebuild to fail: the mark survives => index stays dirty
		// (never fresh-with-a-changed-ledger).
		orig := listRebuildFiles
		listRebuildFiles = func(Config) ([]string, error) { return nil, context.DeadlineExceeded }
		defer func() { listRebuildFiles = orig }()
		var buf bytes.Buffer
		if err := cmdBriefCorrect(context.Background(), []string{"--memory-id", "mem_cite", "--attendee", "person@example.com", "--confirm"}, &buf); err == nil {
			t.Fatal("cmdBriefCorrect returned nil despite a forced rebuild failure")
		}
		if st := gate2IndexState(t, cfg); st == idxFresh {
			t.Fatal("index reads fresh after a governance mutation whose rebuild failed (false-clean)")
		}
	})
}
