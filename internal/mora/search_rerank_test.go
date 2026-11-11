package mora

import (
	"fmt"
	"strings"
	"testing"
)

// seedRerankAudit is the 2026-11-09 audit case: one short written decision and
// three long source files that repeat the query words more often.
func seedRerankAudit(t *testing.T) (Config, string) {
	t.Helper()
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	for i := 0; i < 3; i++ {
		filler := strings.Repeat("quillfern retention policy covers quillfern exports and quillfern audits. ", 12)
		if err := writeMemory(cfg, Memory{
			ID: fmt.Sprintf("fs_quillfern_%d", i), Scope: "personal", Type: "note", Title: fmt.Sprintf("Quillfern handbook part %d", i),
			Source: "filesystem", Provider: "filesystem", CreatedAt: "2026-06-01T00:00:00Z", Text: filler,
		}); err != nil {
			t.Fatal(err)
		}
	}
	dec := Memory{
		ID: newID(), Scope: "personal", Type: "decision", Title: "Quillfern retention decision", Source: "test",
		CreatedAt: "2026-06-02T00:00:00Z", Text: "We decided the quillfern retention window is 30 days.",
	}
	if err := writeMemory(cfg, dec); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(testCtx(t), cfg); err != nil {
		t.Fatal(err)
	}
	return cfg, dec.ID
}

func rerankIDs(mems []Memory) []string {
	ids := make([]string, len(mems))
	for i, m := range mems {
		ids[i] = m.ID
	}
	return ids
}

func TestRerankAuditDecisionFirstFTSPath(t *testing.T) {
	cfg, decID := seedRerankAudit(t)
	var obs searchMemoryObservation
	got, err := searchMemoriesObserved(testCtx(t), cfg, "quillfern retention policy", "", 10, &obs)
	if err != nil {
		t.Fatal(err)
	}
	// Trace.FTS is the order before rerank: the decision is not first there.
	if len(obs.Trace.FTS) < 4 || obs.Trace.FTS[0] == decID {
		t.Fatalf("fixture no longer reproduces the audit: pre-rerank order %v", obs.Trace.FTS)
	}
	if got[0].ID != decID {
		t.Fatalf("after rerank order = %v, want %s first", rerankIDs(got), decID)
	}
	if len(got) != 4 {
		t.Fatalf("rows = %v, want 4", rerankIDs(got))
	}
	// Scores stay the raw BM25 costs of the same rows.
	for _, m := range got {
		if m.Score != obs.BM25[m.ID] {
			t.Fatalf("%s score changed: %v != %v", m.ID, m.Score, obs.BM25[m.ID])
		}
	}
}

func TestRerankAuditDecisionFirstHybridPath(t *testing.T) {
	cfg, decID := seedRerankAudit(t)
	got, tr, err := hybridSearchTrace(testCtx(t), cfg, "quillfern retention policy", "", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Fused) < 4 || tr.Fused[0] == decID {
		t.Fatalf("fixture no longer reproduces the audit: pre-rerank fused order %v", tr.Fused)
	}
	if got[0].ID != decID {
		t.Fatalf("after rerank order = %v, want %s first", rerankIDs(got), decID)
	}
}

// The hook shows at most 2 rows; the decision must be one of them.
func TestRerankAuditDecisionReachesTheHook(t *testing.T) {
	cfg, decID := seedRerankAudit(t)
	got, err := hookRecallSearch(testCtx(t), cfg, "quillfern retention policy", "", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || got[0].ID != decID {
		t.Fatalf("hook rows = %v, want %s first", rerankIDs(got), decID)
	}
}

// With the segment arm fused, hookRecallSearch restores each row's BM25 cost by
// id. A reorder must not move a cost onto another row.
func TestRerankHookRestoresBM25CostPerRow(t *testing.T) {
	cfg := seedGmailSegmentsSearchFixture(t)
	decID := newID()
	if err := writeMemory(cfg, Memory{
		ID: decID, Scope: "personal", Type: "decision", Title: "Kickoff decision " + gsSearchShared, Source: "test",
		CreatedAt: "2026-06-12T00:00:00Z", Text: "We decided on " + gsSearchShared + " for the kickoff.",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(testCtx(t), cfg); err != nil {
		t.Fatal(err)
	}
	query := "kickoff " + gsSearchShared + " " + gsSearchBeta
	var obs searchMemoryObservation
	if _, err := searchMemoriesObserved(testCtx(t), cfg, query, "", 10, &obs); err != nil {
		t.Fatal(err)
	}
	if !obs.ScoreFused {
		t.Fatal("fixture must reach the fused path")
	}
	if len(obs.Trace.Fused) == 0 || obs.Trace.Fused[0] == decID {
		t.Fatalf("fixture must put the decision behind another row before rerank: %v", obs.Trace.Fused)
	}
	got, err := hookRecallSearch(testCtx(t), cfg, query, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < 2 {
		t.Fatalf("rows = %v", rerankIDs(got))
	}
	if got[0].ID != decID {
		t.Fatalf("hook order = %v, want the decision first", rerankIDs(got))
	}
	for _, m := range got {
		if want := obs.BM25[m.ID]; m.Score != want {
			t.Fatalf("%s score = %v, want its own BM25 cost %v", m.ID, m.Score, want)
		}
	}
	// The segment-only row has no parent cost and keeps 0, so a strict
	// threshold still drops it; a reorder must not change that.
	for _, m := range got {
		if _, ok := obs.BM25[m.ID]; !ok && m.Score != 0 {
			t.Fatalf("segment-only row %s score = %v, want 0", m.ID, m.Score)
		}
	}
}

func TestRerankKeepsRowsAndScoresOnBothPaths(t *testing.T) {
	cfg, _ := seedRerankAudit(t)
	var obs searchMemoryObservation
	got, err := searchMemoriesObserved(testCtx(t), cfg, "quillfern retention policy", "", 10, &obs)
	if err != nil {
		t.Fatal(err)
	}
	want := append([]string(nil), obs.Trace.FTS...)
	have := rerankIDs(got)
	if len(have) != len(want) {
		t.Fatalf("rows in %v, rows out %v", want, have)
	}
	seen := map[string]bool{}
	for _, id := range have {
		if seen[id] {
			t.Fatalf("duplicate %s", id)
		}
		seen[id] = true
	}
	for _, id := range want {
		if !seen[id] {
			t.Fatalf("row %s dropped", id)
		}
	}
}
