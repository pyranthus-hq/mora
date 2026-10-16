package mora

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	healthpkg "github.com/pyranthus-hq/mora/internal/health"
	ingestpkg "github.com/pyranthus-hq/mora/internal/ingest"
	"github.com/pyranthus-hq/mora/internal/memory"
)

// index_refreshing_test.go — the refreshing contract (2026-10-15 "index always
// out of sync" investigation). Search-current states must stop reading dirty,
// and the states that genuinely mean the index trails the vault must not.

// TestRebuildSelfOpReadsRefreshingOnlyWhileRunning: a running rebuild's own
// marker is refreshing; the same marker left by a failed rebuild, or by a rebuild
// whose owner died, is dirty.
func TestRebuildSelfOpReadsRefreshingOnlyWhileRunning(t *testing.T) {
	withTempHome(t)
	t.Setenv("MORA_EMBEDDER", "")
	run(t, "init")
	cfg := mustConfig(t)
	ctx := testCtx(t)
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatalf("seed rebuild: %v", err)
	}
	orig := listRebuildFiles
	t.Cleanup(func() { listRebuildFiles = orig })

	// The operation receipts ride the pinned test clock, so health must be read
	// on the same clock the receipts were stamped with.
	var during indexHealth
	var duringOps []pendingOp
	listRebuildFiles = func(c Config) ([]string, error) {
		duringOps, _ = listPendingOps(c)
		during = indexHealthOf(c, c.OperationClock())
		return orig(c)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if len(duringOps) != 1 || duringOps[0].Kind != opKindRebuild || duringOps[0].RunID == "" {
		t.Fatalf("self-op during rebuild = %+v, want one rebuild marker bound to its run id", duringOps)
	}
	if during.State != idxRefreshing || during.PendingOps != 1 {
		t.Fatalf("health during a live rebuild = %+v, want refreshing with its one self-op", during)
	}
	if h := indexHealthOf(cfg, cfg.OperationClock()); h.State != idxFresh {
		t.Fatalf("health after the rebuild committed = %+v, want fresh", h)
	}

	subRun(t, "owner died mid-run", func(t *testing.T) {
		origAlive := operationProcessAlive
		t.Cleanup(func() { operationProcessAlive = origAlive })
		listRebuildFiles = func(c Config) ([]string, error) {
			operationProcessAlive = func(int) bool { return false }
			during = indexHealthOf(c, c.OperationClock())
			operationProcessAlive = origAlive
			return orig(c)
		}
		if _, err := rebuildIndex(ctx, cfg); err != nil {
			t.Fatalf("rebuild: %v", err)
		}
		if during.State != idxDirty {
			t.Fatalf("self-op of a rebuild whose owner is dead = %+v, want dirty", during)
		}
	})

	subRun(t, "failed rebuild leaves a dirty self-op", func(t *testing.T) {
		listRebuildFiles = func(Config) ([]string, error) { return nil, errors.New("injected listing failure") }
		if _, err := rebuildIndex(ctx, cfg); err == nil {
			t.Fatal("rebuild with a failing listing succeeded")
		}
		h := indexHealthOf(cfg, cfg.OperationClock())
		if h.State != idxDirty || h.PendingOps != 1 {
			t.Fatalf("health after a failed rebuild = %+v, want dirty with the stale self-op", h)
		}
		if h.LastError == "" || h.LastAttemptAt == "" {
			t.Fatalf("failed rebuild did not stamp its failure: %+v", h)
		}
		if line := healthBannerFrom(Health{Index: h}); !strings.Contains(line, "DIRTY") {
			t.Fatalf("failed rebuild's self-op must keep the alarm banner, got %q", line)
		}
	})

	subRun(t, "a later success clears the superseded failure stamp", func(t *testing.T) {
		listRebuildFiles = orig
		if _, err := rebuildIndex(ctx, cfg); err != nil {
			t.Fatalf("recovery rebuild: %v", err)
		}
		h := indexHealthOf(cfg, cfg.OperationClock())
		if h.State != idxFresh || h.LastError != "" || h.LastAttemptAt != "" {
			t.Fatalf("health after recovery = state %q last_error %q last_attempt_at %q, want fresh with the failure cleared", h.State, h.LastError, h.LastAttemptAt)
		}
	})
}

// TestFailureStampClearedOnlyAfterCleanup: the failure stamp outlives the commit
// and is cleared only once marker retirement has finished. A crash between the
// two must leave the diagnosis beside the marker it explains.
func TestFailureStampClearedOnlyAfterCleanup(t *testing.T) {
	withTempHome(t)
	t.Setenv("MORA_EMBEDDER", "")
	run(t, "init")
	cfg := mustConfig(t)
	ctx := testCtx(t)
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatalf("seed rebuild: %v", err)
	}
	stampIndexAttemptFailure(cfg, errors.New("database is locked"))
	origRemove := removePendingOpFile
	t.Cleanup(func() { removePendingOpFile = origRemove })
	removals := 0
	stampDuringRetirement := ""
	removePendingOpFile = func(path string) error {
		if removals == 0 {
			stampDuringRetirement = indexHealthOf(cfg, cfg.OperationClock()).LastError
		}
		removals++
		return origRemove(path)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if removals == 0 {
		t.Fatal("the rebuild retired no marker; the ordering witness never ran")
	}
	if stampDuringRetirement == "" {
		t.Fatal("failure stamp was cleared before marker retirement ran")
	}
	if h := indexHealthOf(cfg, cfg.OperationClock()); h.LastError != "" || h.LastAttemptAt != "" {
		t.Fatalf("failure stamp left after a fully successful rebuild: %+v", h)
	}
}

// TestFirstWriteAfterIdleReadsRefreshing: rule 6 measures fts_indexed_at −
// graph_indexed_at, which after a long idle period is the age of the last full
// rebuild, not how long the graph has trailed this write. A refreshing marker
// accounts for that lag, so the write must not read dirty for the reconcile.
func TestFirstWriteAfterIdleReadsRefreshing(t *testing.T) {
	withTempHome(t)
	t.Setenv("MORA_EMBEDDER", "")
	run(t, "init")
	cfg := mustConfig(t)
	ctx := testCtx(t)
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatalf("seed rebuild: %v", err)
	}
	idle := time.Now().Add(-(indexProjectionLagThreshold + 2*time.Hour)).UTC().Format(time.RFC3339)
	db, err := sql.Open("sqlite", rwIndexDSN(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE index_meta SET value=? WHERE key IN ('graph_indexed_at','vectors_indexed_at')`, idle); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if _, err := callMCPTool(ctx, "write_memory", map[string]any{
		"title": "After idle", "text": "afteridleneedle", "scope": "project:idle", "type": "note",
	}); err != nil {
		t.Fatalf("write_memory: %v", err)
	}
	if h := indexHealthOf(cfg, time.Now()); h.State != idxRefreshing {
		t.Fatalf("first write after %s idle = %+v, want refreshing", indexProjectionLagThreshold+2*time.Hour, h)
	}
	// Without a marker to explain it, the same lag is still rule 6's dirty.
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	db, err = sql.Open("sqlite", rwIndexDSN(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE index_meta SET value=? WHERE key='graph_indexed_at'`, idle); err != nil {
		t.Fatal(err)
	}
	if h := indexHealthOf(cfg, time.Now()); h.State != idxDirty {
		t.Fatalf("unexplained projection lag = %+v, want rule 6 dirty", h)
	}
}

// TestAppleCalendarIngestLeavesNoJournalAfterInProcessRebuild is the regression
// for the applecal/applecalendar key mismatch: the run leased "applecalendar"
// but published under provider "applecal", leasing a second key it never
// released, so the in-process covering rebuild kept the journal and the index
// read dirty until another process rebuilt.
func TestAppleCalendarIngestLeavesNoJournalAfterInProcessRebuild(t *testing.T) {
	withTempHome(t)
	t.Setenv("MORA_EMBEDDER", "")
	run(t, "init")
	cfg := mustConfig(t)
	ctx := testCtx(t)
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatalf("seed rebuild: %v", err)
	}
	src := Source{Name: "applecalendar", Type: "applecalendar", Scope: "personal"}
	orig := ingestSourceDispatchFn
	t.Cleanup(func() { ingestSourceDispatchFn = orig })
	ingestSourceDispatchFn = func(_ context.Context, c Config, s Source, _ io.Writer) (sourceIngestResult, error) {
		mm := memory.MappedMemory{
			StableID: "applecal_event/ev1", Provider: "applecal", Type: "event",
			ProviderID: "ev1", Source: "ev1", Title: "Dentist", Body: "applecaljournalneedle",
			ContentHash: "h_ev1", Scope: s.Scope, CreatedAt: "2026-10-15T09:00:00Z",
		}
		wrote, err := writeMappedMemoryDetailed(c, mm)
		if err != nil || !wrote {
			return sourceIngestResult{}, errors.Join(err, errors.New("fixture event not published"))
		}
		return sourceIngestResult{Examined: 1, Materialized: 1}, nil
	}
	if _, err := ingestSourceDetailed(ctx, cfg, src, io.Discard); err != nil {
		t.Fatalf("applecalendar ingest: %v", err)
	}
	if _, err := os.Stat(ingestpkg.LeasePath(cfg, "applecal")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("publish leased the provider-alias key %q: %v", "applecal", err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatalf("in-process covering rebuild: %v", err)
	}
	if dirty, paths, _, err := ingestpkg.JournalStatus(cfg); err != nil || dirty {
		t.Fatalf("journal after the covering rebuild: dirty=%v paths=%d err=%v; want none", dirty, paths, err)
	}
	if h := indexHealthOf(cfg, cfg.OperationClock()); h.State != idxFresh {
		t.Fatalf("index after applecalendar ingest + in-process rebuild = %+v, want fresh", h)
	}
	if hits, err := searchMemories(ctx, cfg, "applecaljournalneedle", "", 8); err != nil || len(hits) != 1 {
		t.Fatalf("published event searchable = %d hits, err=%v; want 1", len(hits), err)
	}
	out := run(t, "doctor", "--json")
	var rep doctorReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("doctor --json: %v\n%s", err, out)
	}
	found := false
	for _, c := range rep.Checks {
		if c.Name == "index_fresh" {
			found = true
			if !c.OK {
				t.Fatalf("doctor index_fresh is not ok after the ingest: %+v", c)
			}
		}
	}
	if !found {
		t.Fatalf("doctor --json has no index_fresh check:\n%s", out)
	}
}

// TestFailedIngestRetiresOnlyItsOwnUnpublishedJournal: a failed run that
// published nothing retires its own header (as the zero-change success path
// does); a failed run that published keeps its journal dirty; and an older
// run's header is never retired by a later run.
func TestFailedIngestRetiresOnlyItsOwnUnpublishedJournal(t *testing.T) {
	src := Source{Name: "mail", Type: "gmail", Account: "work", Scope: "global"}
	key := ingestOperationSourceKey(src)
	setup := func(t *testing.T) (Config, context.Context) {
		withTempHome(t)
		t.Setenv("MORA_EMBEDDER", "")
		run(t, "init")
		cfg := mustConfig(t)
		ctx := testCtx(t)
		if _, err := rebuildIndex(ctx, cfg); err != nil {
			t.Fatalf("seed rebuild: %v", err)
		}
		orig := ingestSourceDispatchFn
		t.Cleanup(func() { ingestSourceDispatchFn = orig })
		return cfg, ctx
	}
	fetchErr := errors.New("injected fetch failure")

	subRun(t, "failure before any publish retires the header", func(t *testing.T) {
		cfg, ctx := setup(t)
		ingestSourceDispatchFn = func(context.Context, Config, Source, io.Writer) (sourceIngestResult, error) {
			return sourceIngestResult{}, fetchErr
		}
		if _, err := ingestSourceDetailed(ctx, cfg, src, io.Discard); !errors.Is(err, fetchErr) {
			t.Fatalf("ingest error = %v, want the fetch failure", err)
		}
		if _, err := os.Stat(ingestpkg.JournalPath(cfg, key)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed unpublished run left its journal: %v", err)
		}
		if h := indexHealthOf(cfg, cfg.OperationClock()); h.State == idxDirty {
			t.Fatalf("index after an unpublished failure = %+v, want not dirty", h)
		}
	})

	subRun(t, "failure after a publish keeps the journal dirty", func(t *testing.T) {
		cfg, ctx := setup(t)
		ingestSourceDispatchFn = func(_ context.Context, c Config, s Source, _ io.Writer) (sourceIngestResult, error) {
			mm := gmailMM("t1", []string{"a@example.com"}, []string{"b@example.com"})
			mm.Account = s.Account
			if _, err := writeMappedMemoryDetailed(c, mm); err != nil {
				return sourceIngestResult{}, err
			}
			// The path line is best-effort; drop it as a lost append would, and
			// report zero, so only the publish count can protect the journal.
			jp := ingestpkg.JournalPath(c, key)
			b, err := os.ReadFile(jp)
			if err != nil {
				return sourceIngestResult{}, err
			}
			header := strings.SplitN(string(b), "\n", 2)[0] + "\n"
			if err := os.WriteFile(jp, []byte(header), 0o644); err != nil {
				return sourceIngestResult{}, err
			}
			return sourceIngestResult{}, fetchErr
		}
		if _, err := ingestSourceDetailed(ctx, cfg, src, io.Discard); !errors.Is(err, fetchErr) {
			t.Fatalf("ingest error = %v, want the fetch failure", err)
		}
		if _, err := os.Stat(ingestpkg.JournalPath(cfg, key)); err != nil {
			t.Fatalf("failed run that published lost its journal: %v", err)
		}
		if h := indexHealthOf(cfg, cfg.OperationClock()); h.State != idxDirty {
			t.Fatalf("index after a published-then-failed run = %+v, want dirty until a rebuild covers it", h)
		}
	})

	subRun(t, "an older run's header is not this run's to retire", func(t *testing.T) {
		cfg, ctx := setup(t)
		// A crashed earlier run: its durable header landed, its path line did not.
		if err := ingestpkg.AppendDurable(ingestpkg.JournalPath(cfg, key), "run "+newID()+" 2026-10-15T00:00:00Z\n"); err != nil {
			t.Fatal(err)
		}
		ingestSourceDispatchFn = func(context.Context, Config, Source, io.Writer) (sourceIngestResult, error) {
			return sourceIngestResult{}, fetchErr
		}
		if _, err := ingestSourceDetailed(ctx, cfg, src, io.Discard); !errors.Is(err, fetchErr) {
			t.Fatalf("ingest error = %v, want the fetch failure", err)
		}
		if _, err := os.Stat(ingestpkg.JournalPath(cfg, key)); err != nil {
			t.Fatalf("a later failed run retired an older run's header: %v", err)
		}
	})
}

// TestSharePullInProgressReadsRefreshingNotDegraded: a normal subscription pull
// must not flash the aggregate red. While a live owned attempt replaces a
// verified generation, the share reads refreshing; a pull that fails still
// reads failed.
func TestSharePullInProgressReadsRefreshingNotDegraded(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	name := "neil"
	registerSub(t, cfg, name)
	dir := packetHFixtureDir(t, cfg, []Memory{fixtureMemory("mem_20260704_000000_aaaaaaaa", "Committed", "durable")})
	if _, err := importFixtureGeneration(context.Background(), cfg, shareSubscription{Name: name, Remote: "r"}, dir); err != nil {
		t.Fatal(err)
	}
	if h := shareHealthOne(cfg, name, time.Now()); h.State != healthFresh {
		t.Fatalf("share after its first pull = %+v, want fresh", h)
	}

	pullErr := errors.New("injected pull failure")
	var mid shareHealth
	var midShares []indexHealth
	var midUnhealthy []shareUnhealth
	var midDoctor string
	err := shareBuildAndPublish(context.Background(), cfg, name, buildModeImport, func(string) (int, error) {
		now := time.Now()
		mid = shareHealthOne(cfg, name, now)
		midShares = shareIndexHealthAll(cfg, now)
		midUnhealthy = sharesUnhealthy(cfg, now)
		midDoctor = run(t, "doctor", "--json")
		return 0, pullErr
	})
	if !errors.Is(err, pullErr) {
		t.Fatalf("pull = %v, want the injected failure", err)
	}
	if mid.State != shareRefreshing {
		t.Fatalf("share during a live pull = %+v, want refreshing", mid)
	}
	if len(midShares) != 1 || midShares[0].State != idxRefreshing {
		t.Fatalf("share index arm during a live pull = %+v, want refreshing", midShares)
	}
	if len(midUnhealthy) != 0 {
		t.Fatalf("shares_unhealthy during a live pull = %+v, want empty", midUnhealthy)
	}
	h := Health{Index: indexHealth{State: idxFresh, Shares: midShares}}
	if got := aggregateHealthState(h); got != healthHealthy {
		t.Fatalf("aggregate during a live pull = %q, want healthy", got)
	}
	if line := healthBannerFrom(h); line != "" {
		t.Fatalf("banner during a live pull = %q, want none", line)
	}
	var rep doctorReport
	if err := json.Unmarshal([]byte(midDoctor), &rep); err != nil {
		t.Fatalf("doctor --json during the pull: %v\n%s", err, midDoctor)
	}
	sawShareCheck := false
	for _, c := range rep.Checks {
		if c.Name == "share_fresh:"+name {
			sawShareCheck = true
			if !c.OK {
				t.Fatalf("doctor share_fresh failed during a normal pull: %+v", c)
			}
		}
	}
	if !sawShareCheck {
		t.Fatalf("doctor --json during the pull has no share_fresh:%s check", name)
	}

	if after := shareHealthOne(cfg, name, time.Now()); after.State != healthFailed {
		t.Fatalf("share after a failed pull = %+v, want failed", after)
	}
}

func TestCompanionIndexStateRefreshingIsHealthy(t *testing.T) {
	if got := companionIndexState(healthpkg.IndexRefreshing); got != companionIndexState(healthpkg.IndexFresh) {
		t.Fatalf("companion state for refreshing = %q, want the fresh mapping %q", got, companionIndexState(healthpkg.IndexFresh))
	}
	if got := companionIndexState(healthpkg.IndexDirty); got == companionIndexState(healthpkg.IndexFresh) {
		t.Fatalf("dirty must not map to the fresh companion state, got %q", got)
	}
}

// TestDoctorIndexFreshPassesWhileRefreshing: right after an authored write the
// index reads refreshing, and doctor's critical index_fresh check passes. The
// typed index arm still reports the state and the pending marker honestly.
func TestDoctorIndexFreshPassesWhileRefreshing(t *testing.T) {
	withTempHome(t)
	t.Setenv("MORA_EMBEDDER", "")
	run(t, "init")
	cfg := mustConfig(t)
	ctx := testCtx(t)
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatalf("seed rebuild: %v", err)
	}
	if _, err := callMCPTool(ctx, "write_memory", map[string]any{
		"title": "Doctor refreshing", "text": "doctorrefreshingneedle", "scope": "project:doc", "type": "note",
	}); err != nil {
		t.Fatalf("write_memory: %v", err)
	}
	out := run(t, "doctor", "--json")
	var rep doctorReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("doctor --json: %v\n%s", err, out)
	}
	if rep.Index.State != idxRefreshing || rep.Index.PendingOps != 1 {
		t.Fatalf("doctor index arm = %+v, want refreshing with the one pending write", rep.Index)
	}
	for _, c := range rep.Checks {
		if c.Name == "index_fresh" && !c.OK {
			t.Fatalf("index_fresh failed while the index is only refreshing: %+v", c)
		}
	}
}

// TestCLIWriteReadsRefreshingDuringReconcile: `mora write` waits for its own
// reconcile. While that rebuild runs, the write's FTS-committed marker and the
// rebuild's live self-op both read refreshing, so a concurrent reader sees no
// false DIRTY.
func TestCLIWriteReadsRefreshingDuringReconcile(t *testing.T) {
	withTempHome(t)
	t.Setenv("MORA_EMBEDDER", "")
	run(t, "init")
	cfg := mustConfig(t)
	if _, err := rebuildIndex(testCtx(t), cfg); err != nil {
		t.Fatalf("seed rebuild: %v", err)
	}
	orig := listRebuildFiles
	t.Cleanup(func() { listRebuildFiles = orig })
	var during []indexHealth
	listRebuildFiles = func(c Config) ([]string, error) {
		during = append(during, indexHealthOf(c, c.OperationClock()))
		return orig(c)
	}
	run(t, "write", "--scope", "project:cli", "--type", "note", "--title", "CLI refreshing", "--text", "clirefreshingneedle")
	if len(during) == 0 {
		t.Fatal("mora write ran no reconcile rebuild")
	}
	for i, h := range during {
		if h.State != idxRefreshing {
			t.Fatalf("health during reconcile pass %d = %+v, want refreshing", i+1, h)
		}
	}
	if h := indexHealthOf(cfg, cfg.OperationClock()); h.State != idxFresh {
		t.Fatalf("health after mora write returned = %+v, want fresh", h)
	}
}
