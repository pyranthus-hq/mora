package mora

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// gate2_health_test.go — Packet B (index health, fail-closed doctor, aggregate
// banner, B4 suppression, B1a manifest, B5 wiki index) acceptance + matrix rows
// 9-13, 16a/16b, 20, 26a/26b, 27, 31, 35, 36.

var gate2Now = time.Unix(1_700_000_000, 0)

// TestIndexHealthFailsClosed (matrix row 10) — indexHealthOf never reads fresh on an
// absent / schema-stale / blocked index. MUTATION: an "assume fine" branch => fresh
// on one of these => RED.
func TestIndexHealthFailsClosed(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		cfg := sandboxCfg(t)
		if st := indexHealthOf(cfg, gate2Now).State; st != idxNever {
			t.Fatalf("absent index state = %q, want never", st)
		}
	})
	t.Run("schema_stale", func(t *testing.T) {
		cfg := gate2Vault(t)
		stampUserVersion(t, cfg, indexSchemaVersion+99)
		if st := indexHealthOf(cfg, gate2Now).State; st != idxFailed {
			t.Fatalf("schema-stale index state = %q, want failed", st)
		}
	})
	t.Run("blocked", func(t *testing.T) {
		cfg := gate2Vault(t)
		if err := writeBlockRecord(cfg, decBlockIdentity, cfg.VaultDir, 5, 3); err != nil {
			t.Fatal(err)
		}
		h := indexHealthOf(cfg, gate2Now)
		if h.State != idxFailed || !h.Blocked {
			t.Fatalf("blocked index = %+v, want failed+Blocked", h)
		}
	})
}

func stampUserVersion(t *testing.T, cfg Config, v int) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("PRAGMA user_version = " + strconv.Itoa(v)); err != nil {
		t.Fatal(err)
	}
}

// TestDirtyIndexIsUnhealthy (matrix row 9) — a pending op makes the index dirty and
// the aggregate unhealthy. MUTATION: indexHealthOf returns fresh with a pending op => RED.
func TestDirtyIndexIsUnhealthy(t *testing.T) {
	cfg := gate2Vault(t)
	if _, err := markIndexDirty(context.Background(), cfg, pendingOp{Kind: opKindWrite, Path: filepath.Join(memoriesRoot(cfg), "global", "pend.md")}); err != nil {
		t.Fatal(err)
	}
	h := healthOf(cfg, gate2Now)
	if h.Index.State != idxDirty {
		t.Fatalf("index state = %q, want dirty", h.Index.State)
	}
	if h.State != healthUnhealthy {
		t.Fatalf("aggregate = %q, want unhealthy", h.State)
	}
}

// TestFreshSourceCannotMaskDirtyIndex (matrix row 11) — the aggregate is worst-of:
// a fresh source cannot mask a dirty index. MUTATION: best-of instead of worst-of => healthy => RED.
func TestFreshSourceCannotMaskDirtyIndex(t *testing.T) {
	h := Health{
		Sources: []sourceHealth{{Key: "gmail", State: healthFresh, AgeHours: 0}},
		Index:   indexHealth{State: idxDirty, PendingOps: 3, DirtySince: gate2Now.UTC().Format(time.RFC3339)},
	}
	if got := aggregateHealthState(h); got != healthUnhealthy {
		t.Fatalf("aggregate(fresh source + dirty index) = %q, want unhealthy", got)
	}
	// The banner reflects the index arm even though the source is fresh.
	banner := healthBannerFrom(h)
	if !strings.Contains(banner, "search index is DIRTY") {
		t.Fatalf("banner = %q, want the index dirty line", banner)
	}
}

// TestDoctorStrictNonzeroOnDirtyIndex (matrix row 12) — doctor --strict is nonzero
// on a dirty index. MUTATION: index_fresh made non-critical => strict exits 0 => RED.
func TestDoctorStrictNonzeroOnDirtyIndex(t *testing.T) {
	cfg := gate2Vault(t)
	if _, err := markIndexDirty(context.Background(), cfg, pendingOp{Kind: opKindWrite, Path: filepath.Join(memoriesRoot(cfg), "global", "pend.md")}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	err := cmdDoctor(context.Background(), []string{"--json", "--strict"}, &buf)
	if err == nil {
		t.Fatal("doctor --strict exited 0 on a dirty index")
	}
	var rep doctorReport
	if jerr := json.Unmarshal(buf.Bytes(), &rep); jerr != nil {
		t.Fatal(jerr)
	}
	if rep.Healthy {
		t.Fatal("doctor report healthy=true on a dirty index")
	}
	if rep.Index.State != idxDirty {
		t.Fatalf("report index.state = %q, want dirty", rep.Index.State)
	}
}

// TestBriefRendersIndexBanner (matrix row 13) — the daily brief renders the index
// arm in its banner. MUTATION: banner drops the index arm => no index line => RED.
func TestBriefRendersIndexBanner(t *testing.T) {
	cfg := gate2Vault(t)
	if _, err := markIndexDirty(context.Background(), cfg, pendingOp{Kind: opKindWrite, Path: filepath.Join(memoriesRoot(cfg), "global", "pend.md")}); err != nil {
		t.Fatal(err)
	}
	d, err := buildDigest(cfg, gate2Now, briefOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if banner := renderDigestHealthBanner(d); !strings.Contains(banner, "search index is DIRTY") {
		t.Fatalf("digest banner = %q, want the index dirty line", banner)
	}
}

// TestPendingDeleteIsNeverServed (matrix rows 16a/16b) — a memory with a pending
// delete op is suppressed on BOTH read chokepoints (search JOIN and
// loadMemoriesByID). MUTATION: drop suppression at either => the deleted id is served => RED.
func TestPendingDeleteIsNeverServed(t *testing.T) {
	cfg := gate2Vault(t, coreBIdxmem("mem_del", "global", "insight", "Secret", "suppressme body"))
	ctx := context.Background()
	// A pending delete for mem_del (its rebuild failed / was killed).
	if _, err := markIndexDirty(ctx, cfg, pendingOp{Kind: opKindDelete, Path: filepath.Join(memoriesRoot(cfg), "global", "mem_del.md"), MemoryID: "mem_del"}); err != nil {
		t.Fatal(err)
	}

	t.Run("search", func(t *testing.T) {
		if res := gate2Search(t, cfg, "suppressme"); len(res) != 0 {
			t.Fatalf("search returned a pending-delete id: %+v", res)
		}
	})
	t.Run("meeting_prep", func(t *testing.T) {
		// loadMemoriesByID is the graph/meeting-prep chokepoint.
		db, err := openIndexRO(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		mems, err := loadMemoriesByID(ctx, cfg, db, []string{"mem_del"})
		if err != nil {
			t.Fatal(err)
		}
		if len(mems) != 0 {
			t.Fatalf("loadMemoriesByID served a pending-delete id: %+v", mems)
		}
	})
	if st := gate2IndexState(t, cfg); st != idxDirty {
		t.Fatalf("index state = %q, want dirty while a delete is pending", st)
	}
}

// TestEmbedderMismatchIsDegraded (matrix row 20) — recorded embedder != configured
// => degraded. MUTATION: drop indexHealthOf rule 5 => fresh => RED.
func TestEmbedderMismatchIsDegraded(t *testing.T) {
	cfg := gate2Vault(t) // recorded: static-hash-v1
	t.Setenv("MORA_EMBEDDER", "ollama")
	h := indexHealthOf(cfg, gate2Now)
	if h.State != idxDegraded {
		t.Fatalf("index state = %q, want degraded (static built, ollama configured)", h.State)
	}
	if h.Embedder.Match {
		t.Fatal("embedder Match=true on a mismatch")
	}
}

// TestOutOfBandVaultEditIsDirty (matrix rows 26a/26b) — an out-of-band edit is
// detected by the content manifest, even when mtime is preserved. MUTATION 26a: skip
// the recompute => never detected. MUTATION 26b: compare mtime => equal-mtime edit reads clean => RED.
func TestOutOfBandVaultEditIsDirty(t *testing.T) {
	cfg := gate2Vault(t, coreBIdxmem("mem_edit", "global", "insight", "Edit", "original body"))
	target := filepath.Join(memoriesRoot(cfg), "global", "mem_edit.md")

	// Precondition: a fresh, committed manifest verifies clean.
	if ok, crit := indexMatchesVault(cfg); !ok || crit {
		t.Fatalf("precondition indexMatchesVault = (%v,%v), want (true,false)", ok, crit)
	}

	t.Run("mtime_not_digest", func(t *testing.T) {
		info, err := os.Stat(target)
		if err != nil {
			t.Fatal(err)
		}
		orig := info.ModTime()
		// Edit the CONTENT but restore the ORIGINAL mtime (a backdated restore / an
		// edit inside one timestamp quantum): an mtime check would read clean.
		if err := os.WriteFile(target, []byte("---\nid: mem_edit\n---\n\ntampered body\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(target, orig, orig); err != nil {
			t.Fatal(err)
		}
		ok, crit := indexMatchesVault(cfg)
		if ok || !crit {
			t.Fatalf("indexMatchesVault after equal-mtime edit = (%v,%v), want (false,true)", ok, crit)
		}
	})
}

// TestWikiIndexTimestampMatchesIndexMeta (matrix row 27) — vault/index.md's Updated:
// stamp equals index_meta.indexed_at, because both come from the same rebuild commit.
// MUTATION: leave writeWikiIndex at its single CLI site => a non-CLI rebuild leaves
// index.md stale and it disagrees with index_meta => RED.
func TestWikiIndexTimestampMatchesIndexMeta(t *testing.T) {
	cfg := gate2Vault(t)
	pinned := time.Unix(1_711_000_000, 0)
	gate2PinClock(t, pinned)
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	metaStamp := gate2ReadMeta(t, cfg)["indexed_at"]
	body, err := os.ReadFile(filepath.Join(cfg.VaultDir, "index.md"))
	if err != nil {
		t.Fatal(err)
	}
	var updated string
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "> Updated:") {
			updated = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "> Updated:"))
		}
	}
	if updated == "" || updated != metaStamp {
		t.Fatalf("index.md Updated %q != index_meta.indexed_at %q", updated, metaStamp)
	}
}

// TestUpsertAdvancesFTSNotGraph (matrix row 35) — an incremental upsert advances
// fts_indexed_at but NOT graph_indexed_at. MUTATION: upsert advances graph too => they
// stay equal => RED.
func TestUpsertAdvancesFTSNotGraph(t *testing.T) {
	cfg := gate2Vault(t)
	before := gate2ReadMeta(t, cfg)
	graphAt := before["graph_indexed_at"]
	if graphAt == "" {
		t.Fatal("no graph_indexed_at after rebuild")
	}
	// A later write advances FTS to a distinct, future instant.
	gate2PinClock(t, time.Unix(1_800_000_000, 0))
	gate2Write(t, cfg, coreBIdxmem("", "global", "insight", "Later", "laterbody"))
	after := gate2ReadMeta(t, cfg)
	if after["graph_indexed_at"] != graphAt {
		t.Fatalf("graph_indexed_at advanced on an upsert: %q -> %q", graphAt, after["graph_indexed_at"])
	}
	if after["fts_indexed_at"] == graphAt {
		t.Fatal("fts_indexed_at did NOT advance past graph on an upsert")
	}
}

// TestProjectionLagUsesStampRelation (matrix row 36) — lag is fts−graph (a relation),
// never wall-clock age. MUTATION: now−graph_indexed_at => an idle vault reddens by aging => RED.
func TestProjectionLagUsesStampRelation(t *testing.T) {
	t.Run("idle_does_not_age", func(t *testing.T) {
		cfg := gate2Vault(t) // fts == graph
		farFuture := time.Unix(2_000_000_000, 0)
		if st := indexHealthOf(cfg, farFuture).State; st != idxFresh {
			t.Fatalf("idle vault state at a far-future now = %q, want fresh (no aging)", st)
		}
	})
	t.Run("authored_write_advances_fts_only", func(t *testing.T) {
		cfg := gate2Vault(t)
		base := gate2ReadMeta(t, cfg)["graph_indexed_at"]
		gt, _ := time.Parse(time.RFC3339, base)
		// A write advances FTS past the graph beyond the threshold.
		gate2PinClock(t, gt.Add(indexProjectionLagThreshold+time.Hour))
		gate2Write(t, cfg, coreBIdxmem("", "global", "insight", "Lagger", "laggerbody"))
		if st := indexHealthOf(cfg, gate2Now).State; st != idxDirty {
			t.Fatalf("index state after fts-advances-past-graph = %q, want dirty (lag)", st)
		}
	})
}

// TestDisabledSourceWithCorpusIsNotHealthy (matrix row 31) — disabling a connector
// over an existing corpus is not green. MUTATION: sources_config keeps OK:len>0
// (counts disabled rows) => healthy => RED.
func TestDisabledSourceWithCorpusIsNotHealthy(t *testing.T) {
	cfg := gate2Vault(t)
	// A gmail corpus exists on disk, but the gmail source is DISABLED.
	gmailDir := filepath.Join(sourcesRoot(cfg), "gmail")
	if err := os.MkdirAll(gmailDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gmailDir, "thread.md"), []byte("---\nid: gmail_t\n---\n\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := saveSources(cfg, []Source{{Name: "gmail", Type: "gmail", Enabled: ptr(false)}}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := cmdDoctor(context.Background(), []string{"--json"}, &buf); err != nil {
		t.Fatal(err)
	}
	var rep doctorReport
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	var sourcesConfigOK, present bool
	for _, c := range rep.Checks {
		if c.Name == "sources_config" {
			present = true
			sourcesConfigOK = c.OK
		}
	}
	if !present {
		t.Fatal("no sources_config check in the report")
	}
	if sourcesConfigOK {
		t.Fatal("sources_config is OK while every source is disabled over an existing corpus")
	}
	if rep.Healthy {
		t.Fatal("doctor healthy=true with a disabled connector over a corpus")
	}
}
