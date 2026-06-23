package mora

// T2 retrieval-recall evaluation (docs/design/2026-06-05-t2-recall-eval-design.md).
//
// Three run modes, layered by the design doc:
//   - TestEvalMetrics    — pure metric-math unit test, no DB. Always runs.
//   - TestEvalSynthetic  — deterministic fixture; gates ONE invariant
//                          (Recall@5[gen=seed,archetype=exact,surface=fts]==1.0),
//                          logs everything else. Runs in CI.
//   - TestEvalLive       — MORA_EVAL_LIVE: per-surface Recall@5/@10 + MRR and the
//                          §6 attribution histogram over the REAL vault, read-only,
//                          report-only. t.Skip in CI.
//   - TestEvalAB         — static-hash vs Ollama on an ISOLATED COPY of the live
//                          vault (re-indexed per embedder). Asserts the vec arm is
//                          non-empty under Ollama before trusting the delta; skips
//                          if the daemon is down. Never gates.
//
// The load-bearing deliverable is the attribution histogram (COVERAGE / RETRIEVAL
// / FUSION / HIT): it settles "static-hash embedder vs corpus gap" mechanically,
// with zero metric math.

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// ---- attribution buckets (§6) ----

const (
	bHIT       = "HIT"
	bFUSION    = "FUSION"
	bRETRIEVAL = "RETRIEVAL"
	bCOVERAGE  = "COVERAGE"
	bNEG       = "NEGCONTROL"
)

// bucketOrder fixes the histogram print order (deterministic, no map ranging).
var bucketOrder = []string{bHIT, bFUSION, bRETRIEVAL, bCOVERAGE, bNEG}

// qmeta carries the per-query golden-set tags (first-seen row wins per qid).
type qmeta struct{ source, archetype, gen, surface string }

// ---- golden-set loading (TREC qrels, tab-separated) ----

// loadQueries reads `qid<TAB>query_text`. Blank and #-comment lines are skipped.
func loadQueries(t *testing.T, path string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, line := range readTSVLines(t, path) {
		f := strings.SplitN(line, "\t", 2)
		if len(f) != 2 {
			t.Fatalf("%s: malformed query line %q (want qid<TAB>text)", path, line)
		}
		out[f[0]] = f[1]
	}
	return out
}

// loadQrels reads `qid iter doc_id rel source archetype gen surface` (tab-sep).
// rel>0 rows become the relevant set; doc_id=NONE rows are kept (negative
// controls). Returns the per-query relevance map, per-query metadata, and the
// sorted qid list (deterministic aggregate order).
func loadQrels(t *testing.T, path string) (map[string]map[string]int, map[string]qmeta, []string) {
	t.Helper()
	rel := map[string]map[string]int{}
	meta := map[string]qmeta{}
	for _, line := range readTSVLines(t, path) {
		f := strings.Split(line, "\t")
		if len(f) < 4 {
			t.Fatalf("%s: qrel line %q has %d cols, want >=4 (qid iter doc_id rel ...)", path, line, len(f))
		}
		qid, doc, relStr := f[0], f[2], f[3]
		g, err := strconv.Atoi(strings.TrimSpace(relStr))
		if err != nil {
			t.Fatalf("%s: bad rel %q in %q: %v", path, relStr, line, err)
		}
		if rel[qid] == nil {
			rel[qid] = map[string]int{}
		}
		rel[qid][doc] = g
		if _, seen := meta[qid]; !seen {
			m := qmeta{}
			if len(f) > 4 {
				m.source = f[4]
			}
			if len(f) > 5 {
				m.archetype = f[5]
			}
			if len(f) > 6 {
				m.gen = f[6]
			}
			if len(f) > 7 {
				m.surface = f[7]
			}
			meta[qid] = m
		}
	}
	qids := make([]string, 0, len(rel))
	for q := range rel {
		qids = append(qids, q)
	}
	sort.Strings(qids)
	return rel, meta, qids
}

func readTSVLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out []string
	for _, ln := range strings.Split(string(b), "\n") {
		s := strings.TrimRight(ln, "\r")
		if strings.TrimSpace(s) == "" || strings.HasPrefix(strings.TrimSpace(s), "#") {
			continue
		}
		out = append(out, s)
	}
	return out
}

// goldIDs returns the rel>0 doc ids for a query (sorted), plus whether this is a
// pure negative-control query (its only labeled doc is NONE).
func goldIDs(relForQ map[string]int) (ids []string, isNeg bool) {
	for id, g := range relForQ {
		if g > 0 && id != "NONE" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		if _, ok := relForQ["NONE"]; ok {
			return nil, true
		}
	}
	return ids, false
}

// ---- attribution (§6 switch, verified against the live tree) ----

// classifyBucket places a gold doc into one §6 bucket for a SINGLE surface — the
// per-surface separation is the spec's #1 guard against "wrong-surface false
// confidence" (an FTS miss recovered by the vector arm must NOT read as HIT on
// the FTS surface). Inputs:
//
//	isNone        — qrel row is a negative control (doc_id=NONE)
//	inIndex       — a memories row exists for the gold id
//	rankInSurface — 0-based rank in THIS surface's full ranked list (-1 if absent)
//	k             — the surface's production cutoff (search_memory=5, context_memory=10)
//	foundByAnyArm — did any arm feeding this surface find the gold id at any depth
//
// COVERAGE is checked before retrieval state so an un-ingested doc is never
// blamed on the embedder (and vice versa) — the COVERAGE↔RETRIEVAL misroute the
// design doc names as a top risk.
func classifyBucket(isNone, inIndex bool, rankInSurface, k int, foundByAnyArm bool) string {
	switch {
	case isNone:
		return bNEG
	case !inIndex:
		return bCOVERAGE // never ingested → fix = connector (fuzzy-check the fact first)
	case rankInSurface >= 0 && rankInSurface < k:
		return bHIT
	case foundByAnyArm:
		return bFUSION // an arm found it; the surface buried it past k (RRF / pool / rank)
	default:
		return bRETRIEVAL // indexed, no arm surfaced it (paraphrase ⇒ embedder)
	}
}

// ---- reporting ----

const (
	kFTS           = mcpSearchDefaultLimit // search_memory default cutoff — coupled to production so they can't drift
	kHybrid        = 10                    // context_memory hybridSearch cutoff
	tracePoolDepth = 200                   // > production pool=limit*5; separates FUSION from RETRIEVAL
)

// evalReport is reportEval's machine-readable summary so callers can gate on it.
type evalReport struct {
	histFTS, histHybrid map[string]int
	scored, negCount    int
}

// reportEval prints the per-query attribution, a PER-SURFACE §6 histogram (FTS
// vs hybrid — the wrong-surface-confidence guard), and a surface-honest
// Recall/MRR table. Metrics are scored over each surface's PRODUCTION-EMITTED
// list (search_memory@5, context_memory@10) so a doc the surface never returns
// can never earn recall or reciprocal rank. Report-only; gating is the caller's.
func reportEval(t *testing.T, ctx context.Context, cfg Config, db *sql.DB, queries map[string]string, rel map[string]map[string]int, meta map[string]qmeta, qids []string) evalReport {
	t.Helper()
	histFTS, histHybrid := map[string]int{}, map[string]int{}
	var recallQids []string
	ftsR5, ftsMRR := map[string]float64{}, map[string]float64{}
	hybR5, hybR10, hybMRR := map[string]float64{}, map[string]float64{}, map[string]float64{}
	negCount := 0

	t.Logf("=== T2 per-query attribution (k_fts=%d, k_hyb=%d, tracePool=%d) ===", kFTS, kHybrid, tracePoolDepth)
	for _, qid := range qids {
		q, ok := queries[qid]
		if !ok {
			t.Logf("WARN %s: qrel has no query text in queries file — skipped", qid)
			continue
		}
		ftsRanked := mustSearchIDs(t, ctx, cfg, q, kFTS) // search_memory's real top-5 (surface-honest)
		out, tr := mustHybridTrace(t, ctx, cfg, q, kHybrid, tracePoolDepth)
		hybRanked := idList(out) // context_memory's real top-10 (surface-honest)

		golds, isNeg := goldIDs(rel[qid])
		if isNeg {
			negCount++
			t.Logf("%s %-44q NEGCONTROL | fts_top=%v hyb_top=%v | abstention (excluded from recall)", qid, q, head(ftsRanked, 3), head(hybRanked, 3))
			continue
		}
		recallQids = append(recallQids, qid)
		ftsR5[qid] = recallAtK(ftsRanked, rel[qid], kFTS)
		ftsMRR[qid] = reciprocalRank(ftsRanked, rel[qid])
		hybR5[qid] = recallAtK(hybRanked, rel[qid], kFTS)
		hybR10[qid] = recallAtK(hybRanked, rel[qid], kHybrid)
		hybMRR[qid] = reciprocalRank(hybRanked, rel[qid])

		m := meta[qid]
		for _, g := range golds {
			inIdx, err := existsInMemoriesTable(ctx, db, g)
			if err != nil {
				t.Fatalf("existsInMemoriesTable(%s): %v", g, err)
			}
			rFTS, rVec, rGraph, rFused := rankOf(g, tr.FTS), rankOf(g, tr.Vec), rankOf(g, tr.Graph), rankOf(g, tr.Fused)
			ftsBucket := classifyBucket(false, inIdx, rFTS, kFTS, rFTS >= 0)
			hybBucket := classifyBucket(false, inIdx, rFused, kHybrid, rFTS >= 0 || rVec >= 0 || rGraph >= 0)
			histFTS[ftsBucket]++
			histHybrid[hybBucket]++
			t.Logf("%s %-44q gold=%-28s | FTS %-9s(fts#%s) HYB %-9s(fused#%s) arms[fts=%d vec=%d graph=%d] [src=%s arch=%s gen=%s surf=%s]",
				qid, q, g, ftsBucket, rankStr(rFTS), hybBucket, rankStr(rFused),
				rFTS, rVec, rGraph, m.source, m.archetype, m.gen, m.surface)
		}
	}

	t.Logf("=== §6 attribution — PER SURFACE (the wrong-surface-confidence guard) ===")
	t.Logf("search_memory (FTS-only, k=%d) : %s", kFTS, fmtHist(histFTS))
	t.Logf("context_memory (hybrid,  k=%d) : %s", kHybrid, fmtHist(histHybrid))
	if negCount > 0 {
		t.Logf("negative controls: %d (abstention — measured separately, excluded from recall)", negCount)
	}

	t.Logf("=== per-surface recall (n=%d scored queries; surface-honest cutoffs) ===", len(recallQids))
	t.Logf("search_memory (FTS-only)  Recall@%d=%.3f  MRR@%d=%.3f", kFTS,
		meanBy(recallQids, func(q string) float64 { return ftsR5[q] }), kFTS,
		meanBy(recallQids, func(q string) float64 { return ftsMRR[q] }))
	t.Logf("context_memory (hybrid)   Recall@%d=%.3f  Recall@%d=%.3f  MRR@%d=%.3f", kFTS,
		meanBy(recallQids, func(q string) float64 { return hybR5[q] }), kHybrid,
		meanBy(recallQids, func(q string) float64 { return hybR10[q] }), kHybrid,
		meanBy(recallQids, func(q string) float64 { return hybMRR[q] }))
	return evalReport{histFTS: histFTS, histHybrid: histHybrid, scored: len(recallQids), negCount: negCount}
}

// bucketHistogram returns the HYBRID-surface §6 histogram (where an embedder
// swap shows up, since Ollama changes the vec arm) PLUS the total vec-arm hits
// across the query set. The A/B reads the RETRIEVAL→HIT migration from the
// histogram and uses vecHits>0 to PROVE the vector arm is actually live — not
// silently degraded to an empty/mismatched arm despite rows being stored.
func bucketHistogram(t *testing.T, ctx context.Context, cfg Config, queries map[string]string, rel map[string]map[string]int, qids []string) (hist map[string]int, vecHits int) {
	t.Helper()
	db := openRO(t, cfg)
	defer db.Close()
	hist = map[string]int{}
	for _, qid := range qids {
		q, ok := queries[qid]
		if !ok {
			continue
		}
		_, tr := mustHybridTrace(t, ctx, cfg, q, kHybrid, tracePoolDepth)
		vecHits += len(tr.Vec)
		golds, isNeg := goldIDs(rel[qid])
		if isNeg {
			continue
		}
		for _, g := range golds {
			inIdx, err := existsInMemoriesTable(ctx, db, g)
			if err != nil {
				t.Fatalf("existsInMemoriesTable(%s): %v", g, err)
			}
			rFTS, rVec, rGraph := rankOf(g, tr.FTS), rankOf(g, tr.Vec), rankOf(g, tr.Graph)
			hist[classifyBucket(false, inIdx, rankOf(g, tr.Fused), kHybrid, rFTS >= 0 || rVec >= 0 || rGraph >= 0)]++
		}
	}
	return hist, vecHits
}

// ---- small helpers ----

func openRO(t *testing.T, cfg Config) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath(cfg)+"?mode=ro")
	if err != nil {
		t.Fatalf("open ro db %s: %v", dbPath(cfg), err)
	}
	return db
}

func mustSearchIDs(t *testing.T, ctx context.Context, cfg Config, query string, limit int) []string {
	t.Helper()
	mems, err := searchMemories(ctx, cfg, query, "", limit)
	if err != nil {
		t.Fatalf("searchMemories(%q): %v", query, err)
	}
	return idList(mems)
}

func mustHybridTrace(t *testing.T, ctx context.Context, cfg Config, query string, limit, tracePool int) ([]Memory, retrievalTrace) {
	t.Helper()
	mems, tr, err := hybridSearchTrace(ctx, cfg, query, "", limit, tracePool)
	if err != nil {
		t.Fatalf("hybridSearchTrace(%q): %v", query, err)
	}
	return mems, tr
}

// liveCfgOrSkip resolves the config for the live diagnosis. MORA_EVAL_LIVE=1
// (or "true") uses the user's real Mora config; any other value is treated as a
// path to a data dir containing index.db (honoring an explicit target instead
// of silently ignoring it). Unset → skip.
func liveCfgOrSkip(t *testing.T) Config {
	t.Helper()
	v := os.Getenv("MORA_EVAL_LIVE")
	if v == "" {
		t.Skip("set MORA_EVAL_LIVE=1 (your Mora config) or =/path/to/datadir (containing index.db) to score the real vault read-only; needs internal/mora/live_{queries,qrels}.tsv (gitignored; see design doc §5)")
	}
	if v == "1" || v == "true" {
		cfg, err := loadConfig()
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		return cfg
	}
	cfg := Config{DataDir: v}
	if _, err := os.Stat(dbPath(cfg)); err != nil {
		t.Fatalf("MORA_EVAL_LIVE=%q: want '1' (use your Mora config) or a data dir containing index.db, but %s not found: %v", v, dbPath(cfg), err)
	}
	return cfg
}

// dirHasAny reports whether dir contains any of the named subdirectories.
func dirHasAny(dir string, subs ...string) bool {
	for _, s := range subs {
		if fi, err := os.Stat(filepath.Join(dir, s)); err == nil && fi.IsDir() {
			return true
		}
	}
	return false
}

func rankStr(rank int) string {
	if rank < 0 {
		return "-"
	}
	return strconv.Itoa(rank)
}

func head(ids []string, n int) []string {
	if len(ids) <= n {
		return ids
	}
	return ids[:n]
}

func fmtHist(h map[string]int) string {
	var parts []string
	for _, b := range bucketOrder {
		if b == bNEG {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%d", b, h[b]))
	}
	return strings.Join(parts, " ")
}

// copyTree deep-copies a directory tree (markdown vault) so the A/B can
// rebuildIndex per embedder WITHOUT touching the live index.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatalf("copyTree %s -> %s: %v", src, dst, err)
	}
}

// evalFixtureMemories is the deterministic synthetic vault the committed golden
// set scores against — the SINGLE source of truth for the fixture (seedEvalFixture
// writes it; the MMR-sensitivity precondition test embeds docs from it by id). All
// dates are fixed, no time.Now / no randomness, so buildGraph + the static embedder
// produce byte-identical output every run.
//
// Stratification (so the aggregate is sensitive, not coarse):
//   - exact/fts (q1, q6)    — verbatim phrase; FTS must HIT (the gated invariants).
//   - person/graph (q2, q7) — body shares no query word; reached ONLY via the
//     sender→gazetteer graph arm, so the FTS-only vs hybrid gap is visible.
//   - topic/paraphrase (q3) — query shares no token with the doc (hybrid recovery).
//   - near-dup cluster (q5) — two heavily-overlapping RELEVANT docs (migration-1/2)
//     that provide deterministic near-dup MATERIAL for the future MMR gate (W2);
//     TestEvalFixtureNearDupPrecondition locks the high-cosine property. NOTE: this
//     is material, NOT a self-contained detector — under the committed static-hash
//     eval q5's fused pool is only ~3 docs vs k=8/10, so an MMR demotion stays in-k
//     and cannot move Recall@k/MRR (and if W2 follows the useVec precedent in
//     hybrid.go and skips reranking under static-hash, MMR never runs here at all).
//     The MMR before/after regression gate is W2's, and belongs in the semantic
//     (Ollama) AB path or needs a larger q5 distractor pool — see the q5 doc below.
//   - decoys                — lexically disjoint noise so recall isn't trivially 1.0.
func evalFixtureMemories() []Memory {
	return []Memory{
		// q1 — exact phrase verbatim in the body; FTS must return it (the gated invariant).
		{ID: "synth/oauth-exact", Scope: "global", Type: "decision", Title: "OAuth design decision",
			CreatedAt: "2026-01-01T00:00:00Z", Source: "obsidian",
			Text: "We decided to use the PKCE authorization code flow for the Wink API."},
		// q2 — person query reaches this ONLY via the graph arm (body shares no query words).
		// from=sender ⇒ trusted alias "Neil Patel" in the gazetteer.
		{ID: "synth/venue", Scope: "global", Type: "email", Title: "Q3 offsite logistics",
			CreatedAt: "2026-01-02T00:00:00Z", Source: "gmail",
			Text: "Booking the venue and catering for the team gathering.",
			Meta: map[string]any{
				"from":  []string{"neil@example.com"},
				"to":    []string{"adit@x.com"},
				"names": map[string]string{"neil@example.com": "Neil Patel"},
			}},
		// q3 — paraphrastic: the query shares NO token with this title/body, so the FTS
		// arm misses it (Recall@5[fts]=0). On THIS small corpus the static-hash vector
		// arm still surfaces it via subword (char-trigram) overlap, so hybrid HITs —
		// which is the point: hybrid recovers what FTS-only drops. A genuine RETRIEVAL
		// miss (static-hash failing on *meaning*) needs the larger, lexically-diverse
		// live vault to appear — which is why the doc reads that verdict there, not here.
		{ID: "synth/runway", Scope: "global", Type: "note", Title: "Q1 budget review",
			CreatedAt: "2026-01-03T00:00:00Z", Source: "obsidian",
			Text: "We extended the cash runway by trimming the marketing spend this quarter."},
		// q5 — near-duplicate RELEVANT pair: deterministic MATERIAL for the future MMR
		// gate (W2), not a self-contained detector. The two bodies share almost every
		// token, so their static-hash vectors are highly similar (cosine≈0.82, locked by
		// TestEvalFixtureNearDupPrecondition), giving a greedy MMR a real redundancy to
		// act on. CAVEAT for W2: under the committed synthetic eval (MORA_EMBEDDER="",
		// useVec=false) q5's fused pool is just these two docs (+1), well inside k=8/10,
		// so an MMR demotion here cannot move Recall@k/MRR. The MMR before/after gate
		// therefore belongs in the semantic (Ollama) AB path, or needs a larger q5
		// distractor pool sharing this vocab so a demotion can cross k.
		{ID: "synth/migration-1", Scope: "global", Type: "note", Title: "Database migration weekend",
			CreatedAt: "2026-01-04T00:00:00Z", Source: "obsidian",
			Text: "We migrated the Postgres database to the new cluster over the weekend."},
		{ID: "synth/migration-2", Scope: "global", Type: "note", Title: "Postgres cluster cutover",
			CreatedAt: "2026-01-05T00:00:00Z", Source: "obsidian",
			Text: "The Postgres database migration to the new cluster finished over the weekend."},
		// q6 — second exact-phrase query; distinct verbatim tokens (standup, 9:30) so it
		// HITs FTS at rank 0. Broadens the gated exact/fts/seed family to two queries.
		{ID: "synth/standup", Scope: "global", Type: "note", Title: "Standup time change",
			CreatedAt: "2026-01-06T00:00:00Z", Source: "obsidian",
			Text: "The daily standup moved to 9:30 AM starting Monday."},
		// q7 — second person query; like q2 the body shares no query token, so it is
		// reached ONLY via the sender→gazetteer graph arm ("Maya Chen"). The id is
		// kept opaque (no "onboarding" token) because rebuildIndex also indexes the
		// memory id into FTS — a query keyword inside the id would forge an FTS hit
		// and defeat the graph-only stratification (real connector ids are opaque).
		{ID: "synth/newhire", Scope: "global", Type: "email", Title: "New hire logistics",
			CreatedAt: "2026-01-07T00:00:00Z", Source: "gmail",
			Text: "Reviewing the new hire schedule and the first-week checklist.",
			Meta: map[string]any{
				"from":  []string{"maya@example.com"},
				"to":    []string{"adit@x.com"},
				"names": map[string]string{"maya@example.com": "Maya Chen"},
			}},
		// decoys — lexically disjoint noise so recall isn't trivially 1.0.
		{ID: "synth/decoy-a", Scope: "global", Type: "note", Title: "Grocery list",
			CreatedAt: "2026-01-08T00:00:00Z", Source: "obsidian", Text: "milk eggs bread butter coffee"},
		{ID: "synth/decoy-b", Scope: "global", Type: "note", Title: "Weekend plans",
			CreatedAt: "2026-01-09T00:00:00Z", Source: "obsidian", Text: "hiking trail and a picnic by the lake"},
		{ID: "synth/decoy-c", Scope: "global", Type: "note", Title: "Reading notes",
			CreatedAt: "2026-01-10T00:00:00Z", Source: "obsidian", Text: "chapter three covers distributed consensus and quorum"},
		{ID: "synth/decoy-d", Scope: "global", Type: "note", Title: "Gym schedule",
			CreatedAt: "2026-01-11T00:00:00Z", Source: "obsidian", Text: "monday yoga wednesday spin friday rest day"},
		{ID: "synth/decoy-e", Scope: "global", Type: "note", Title: "Recipe idea",
			CreatedAt: "2026-01-12T00:00:00Z", Source: "obsidian", Text: "roast the vegetables with olive oil and rosemary"},
	}
}

// seedEvalFixture writes the deterministic synthetic vault to cfg's vault.
func seedEvalFixture(t *testing.T, cfg Config) {
	t.Helper()
	for _, m := range evalFixtureMemories() {
		if err := writeMemory(cfg, m); err != nil {
			t.Fatalf("seed %s: %v", m.ID, err)
		}
	}
}

// evalFixtureByID returns the fixture memory with the given id (fatal if absent),
// so a test can embed the exact title+text writeVectors indexes.
func evalFixtureByID(t *testing.T, id string) Memory {
	t.Helper()
	for _, m := range evalFixtureMemories() {
		if m.ID == id {
			return m
		}
	}
	t.Fatalf("eval fixture has no memory %q", id)
	return Memory{}
}

// ---- tests ----

// TestEvalMetrics locks the metric math independent of any DB/fixture.
func TestEvalMetrics(t *testing.T) {
	rel := map[string]int{"a": 2, "b": 1, "x": 0} // relevant = {a,b}
	ranked := []string{"z", "a", "y", "b"}        // a@rank2, b@rank4
	cases := []struct {
		name string
		got  float64
		want float64
	}{
		{"recall@2", recallAtK(ranked, rel, 2), 0.5}, // top2={z,a} → 1/2
		{"recall@4", recallAtK(ranked, rel, 4), 1.0}, // both found
		{"recall@10", recallAtK(ranked, rel, 10), 1.0},
		{"hit@1", hitAtK(ranked, rel, 1), 0},      // top1=z
		{"hit@2", hitAtK(ranked, rel, 2), 1},      // a in top2
		{"mrr", reciprocalRank(ranked, rel), 0.5}, // first rel (a) at rank2 → 1/2
		{"recall-no-relevant", recallAtK(ranked, map[string]int{"x": 0}, 5), 0},
		{"hit-none", hitAtK([]string{"z", "y"}, rel, 5), 0},
		{"mrr-none", reciprocalRank([]string{"z", "y"}, rel), 0},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if got := rankOf("b", ranked); got != 3 {
		t.Errorf("rankOf(b) = %d, want 3", got)
	}
	if got := rankOf("missing", ranked); got != -1 {
		t.Errorf("rankOf(missing) = %d, want -1", got)
	}
	// meanBy is deterministic regardless of input order.
	v := map[string]float64{"q2": 1, "q1": 0, "q3": 0.5}
	want := (0.0 + 1.0 + 0.5) / 3.0
	if got := meanBy([]string{"q3", "q1", "q2"}, func(q string) float64 { return v[q] }); got != want {
		t.Errorf("meanBy = %v, want %v", got, want)
	}
	if got := meanBy(nil, func(string) float64 { return 1 }); got != 0 {
		t.Errorf("meanBy(empty) = %v, want 0", got)
	}
}

// TestClassifyBucket deterministically exercises every branch of the §6
// attribution switch with pure inputs — the load-bearing logic that settles
// embedder-vs-coverage. Independent of embedder quirks, so a COVERAGE↔RETRIEVAL
// misroute (a named top risk: it sends work to the wrong fix) can never slip
// through CI.
func TestClassifyBucket(t *testing.T) {
	const k = 10
	cases := []struct {
		name          string
		isNone        bool
		inIndex       bool
		rank          int
		foundByAnyArm bool
		want          string
	}{
		{"negcontrol", true, false, -1, false, bNEG},
		{"coverage", false, false, -1, false, bCOVERAGE},
		{"coverage-beats-rank", false, false, 0, true, bCOVERAGE}, // not-in-index dominates
		{"hit-top", false, true, 0, true, bHIT},
		{"hit-boundary", false, true, k - 1, true, bHIT},
		{"fusion-beyond-k", false, true, k, true, bFUSION},    // surface ranked it past k
		{"fusion-not-ranked", false, true, -1, true, bFUSION}, // absent from surface, an arm found it
		{"retrieval", false, true, -1, false, bRETRIEVAL},     // in index, no arm found it
	}
	for _, c := range cases {
		if got := classifyBucket(c.isNone, c.inIndex, c.rank, k, c.foundByAnyArm); got != c.want {
			t.Errorf("%s: classifyBucket = %s, want %s", c.name, got, c.want)
		}
	}
}

// TestExistsInMemoriesTable verifies the COVERAGE probe distinguishes a present
// row from an absent one (false,nil) — so a genuine DB error surfaces instead of
// masquerading as COVERAGE.
func TestExistsInMemoriesTable(t *testing.T) {
	t.Setenv("MORA_EMBEDDER", "")
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	seedEvalFixture(t, cfg)
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatalf("rebuildIndex: %v", err)
	}
	db := openRO(t, cfg)
	defer db.Close()
	if ok, err := existsInMemoriesTable(ctx, db, "synth/oauth-exact"); err != nil || !ok {
		t.Errorf("exists(synth/oauth-exact) = (%v, %v), want (true, nil)", ok, err)
	}
	if ok, err := existsInMemoriesTable(ctx, db, "synth/never-ingested"); err != nil || ok {
		t.Errorf("exists(synth/never-ingested) = (%v, %v), want (false, nil)", ok, err)
	}
}

// TestEvalSynthetic builds the deterministic fixture, reports the full eval, and
// gates exactly ONE hard invariant: an exact-phrase query for a phrase that
// verbatim exists must return its doc on the FTS surface (Recall@5 == 1.0). If
// that breaks, FTS itself is broken. Everything else is logged, not gated —
// you cannot freeze a recall floor blind, before the live numbers exist.
func TestEvalSynthetic(t *testing.T) {
	t.Setenv("MORA_EMBEDDER", "") // a dev with Ollama opted-in must still get the CI number
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	seedEvalFixture(t, cfg)
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatalf("rebuildIndex: %v", err)
	}

	dir := filepath.Join("testdata", "eval")
	queries := loadQueries(t, filepath.Join(dir, "golden_queries.tsv"))
	rel, meta, qids := loadQrels(t, filepath.Join(dir, "golden_qrels.tsv"))

	db := openRO(t, cfg)
	defer db.Close()
	rep := reportEval(t, ctx, cfg, db, queries, rel, meta, qids)
	if rep.negCount == 0 {
		t.Fatal("golden set must include a NONE negative-control row (q4) to regression-test the abstention/exclusion path in reportEval")
	}

	// THE GATE: Recall@5[gen=seed, archetype=exact, surface=fts] == 1.0.
	gated := 0
	for _, qid := range qids {
		m := meta[qid]
		if m.gen == "seed" && m.archetype == "exact" && m.surface == "fts" {
			gated++
			ranked := mustSearchIDs(t, ctx, cfg, queries[qid], kFTS)
			if r := recallAtK(ranked, rel[qid], kFTS); r != 1.0 {
				t.Fatalf("INVARIANT BROKEN: Recall@%d[fts,exact,seed] for %s %q = %.2f, want 1.0 — exact-phrase FTS is broken. ranked=%v", kFTS,
					qid, queries[qid], r, ranked)
			}
		}
	}
	if gated == 0 {
		t.Fatal("golden set has no gen=seed,archetype=exact,surface=fts query — the one gated invariant is missing")
	}
}

// TestEvalFixtureNearDupPrecondition locks the NECESSARY (not sufficient) precondition
// for the future MMR gate (W2): the q5 pair must be a genuine near-dup under the
// static-hash embedder (high cosine) AND lexically far from a decoy (low cosine), and
// both must be retrieved pre-rerank so MMR has both as candidates. This guarantees MMR
// will have a real redundancy to act on; it does NOT by itself make a regression
// observable — under the committed static-hash eval q5's fused pool is smaller than k,
// so a demotion stays in-k (see the q5 fixture comment; that observability is W2's
// job). If a future fixture edit erodes the near-dup overlap, this fails first. Pure
// static-hash (no Ollama) so it is deterministic in CI.
func TestEvalFixtureNearDupPrecondition(t *testing.T) {
	t.Setenv("MORA_EMBEDDER", "")
	emb := defaultEmbedder()
	embedDoc := func(id string) []float32 {
		m := evalFixtureByID(t, id)
		return emb.Embed(m.Title + "\n" + m.Text) // exactly what writeVectors embeds
	}
	dup := cosine(embedDoc("synth/migration-1"), embedDoc("synth/migration-2"))
	far := cosine(embedDoc("synth/migration-1"), embedDoc("synth/decoy-a"))
	if dup < 0.5 {
		t.Errorf("near-dup precondition broken: cosine(migration-1,migration-2)=%.3f, want >=0.5 — MMR would have nothing redundant to demote", dup)
	}
	if far > 0.25 {
		t.Errorf("decoy too similar: cosine(migration-1,decoy-a)=%.3f, want <=0.25 — the near-dup signal must be specific", far)
	}
	if dup <= far {
		t.Errorf("near-dup (%.3f) must exceed decoy similarity (%.3f)", dup, far)
	}

	// Both near-dup gold docs must be retrieved pre-rerank so MMR has both as
	// candidates (the necessary precondition). NOTE: in the committed static-hash eval
	// the fused pool is smaller than k, so this alone does NOT make a demotion
	// observable as a Recall@k drop — that is W2's job (semantic AB path or a larger
	// q5 pool). See the q5 fixture comment.
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	seedEvalFixture(t, cfg)
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatalf("rebuildIndex: %v", err)
	}
	mems, err := hybridSearch(ctx, cfg, "Postgres database migration to the new cluster", "", kHybrid)
	if err != nil {
		t.Fatalf("hybridSearch(q5): %v", err)
	}
	got := map[string]bool{}
	for _, m := range mems {
		got[m.ID] = true
	}
	for _, id := range []string{"synth/migration-1", "synth/migration-2"} {
		if !got[id] {
			t.Errorf("near-dup gold %s missing from hybrid top-%d (got %v) — MMR-off baseline is not 1.0", id, kHybrid, idList(mems))
		}
	}
}

// TestEvalLive scores the REAL vault read-only and prints the §6 histogram. Opt
// in with MORA_EVAL_LIVE=1; requires hand-labeled internal/mora/live_qrels.tsv +
// live_queries.tsv (gitignored). Never rebuilds the live index. This is where
// the embedder-vs-coverage verdict is read.
func TestEvalLive(t *testing.T) {
	cfg := liveCfgOrSkip(t)
	if _, err := os.Stat(dbPath(cfg)); err != nil {
		t.Fatalf("no live index at %s — run `mora index rebuild` first (this test never rebuilds the live vault): %v", dbPath(cfg), err)
	}
	qPath, rPath := "live_queries.tsv", "live_qrels.tsv"
	if _, err := os.Stat(rPath); err != nil {
		t.Skipf("hand-label %s (+ %s) with your vault's gold ids to run the live diagnosis — see design doc §5 (3 RED seeds → ~15 stratified queries)", rPath, qPath)
	}
	ctx := context.Background()
	queries := loadQueries(t, qPath)
	rel, meta, qids := loadQrels(t, rPath)
	db := openRO(t, cfg)
	defer db.Close()
	t.Logf("LIVE eval against %s (query embedder=%s)", dbPath(cfg), chooseEmbedder().ModelID())
	reportEval(t, ctx, cfg, db, queries, rel, meta, qids)
}

// TestEvalAB runs the static-hash-vs-Ollama A/B on an ISOLATED COPY of the live
// vault, re-indexing under each embedder. The headline is the bucket migration
// (RETRIEVAL→HIT proves the gain is semantic). Two correctness guards from the
// design doc: re-index per embedder (vectors are keyed by ModelID), and t.Fatal
// if the vec arm is empty under Ollama (chooseEmbedder silently degrades to
// static → the A/B would compare static-vs-static, the most dangerous bug).
// Skips entirely if the daemon is down — it never gates.
func TestEvalAB(t *testing.T) {
	if os.Getenv("MORA_EVAL_LIVE") == "" {
		t.Skip("set MORA_EVAL_LIVE=1 (+ live qrels + a running Ollama daemon) to run the static-vs-ollama A/B")
	}
	qPath, rPath := "live_queries.tsv", "live_qrels.tsv"
	if _, err := os.Stat(rPath); err != nil {
		t.Skipf("hand-label %s (+ %s) first — the A/B needs gold labels", rPath, qPath)
	}
	// Probe Ollama WITHOUT mutating anything; degrade-to-static ⇒ skip (never fatal here).
	t.Setenv("MORA_EMBEDDER", "ollama")
	ollamaModel := chooseEmbedder().ModelID()
	if !strings.HasPrefix(ollamaModel, "ollama:") {
		t.Skipf("Ollama daemon unreachable (embedder resolved to %q) — A/B needs it; skipping (never gates)", ollamaModel)
	}

	realCfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	srcVault := realCfg.VaultDir
	if v := os.Getenv("MORA_EVAL_LIVE"); v != "1" && v != "true" && dirHasAny(v, "memories", "sources") {
		srcVault = v // an explicit vault path was supplied
	}
	if !dirHasAny(srcVault, "memories", "sources") {
		t.Skipf("no live vault markdown at %s (expected memories/ or sources/) to copy", srcVault)
	}

	ctx := context.Background()
	queries := loadQueries(t, qPath)
	rel, _, qids := loadQrels(t, rPath)

	// Isolated copy — NEVER rebuild the live index.
	withTempHome(t)
	run(t, "init")
	tmpCfg := mustConfig(t)
	for _, sub := range []string{"memories", "sources"} {
		s := filepath.Join(srcVault, sub)
		if _, err := os.Stat(s); err == nil {
			copyTree(t, s, filepath.Join(tmpCfg.VaultDir, sub))
		}
	}

	// Arm 1 — static-hash.
	t.Setenv("MORA_EMBEDDER", "")
	if _, err := rebuildIndex(ctx, tmpCfg); err != nil {
		t.Fatalf("rebuildIndex (static): %v", err)
	}
	staticHist, _ := bucketHistogram(t, ctx, tmpCfg, queries, rel, qids)

	// Arm 2 — Ollama (re-indexed; vectors keyed by the new ModelID).
	t.Setenv("MORA_EMBEDDER", "ollama")
	if m := chooseEmbedder().ModelID(); m != ollamaModel {
		t.Skipf("Ollama embedder changed/degraded mid-test (%q → %q) — aborting A/B (never gates)", ollamaModel, m)
	}
	if _, err := rebuildIndex(ctx, tmpCfg); err != nil {
		t.Fatalf("rebuildIndex (ollama): %v", err)
	}
	db := openRO(t, tmpCfg)
	var nVec int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM mem_vectors WHERE model = ?`, ollamaModel).Scan(&nVec); err != nil {
		db.Close()
		t.Fatalf("vec count for %q: %v", ollamaModel, err)
	}
	db.Close()
	if nVec == 0 {
		t.Fatalf("A/B INVALID: 0 vectors stored for model %q after re-index — embedding failed or degraded to static; would compare static-vs-static", ollamaModel)
	}
	ollamaHist, vecHits := bucketHistogram(t, ctx, tmpCfg, queries, rel, qids)
	// Verify Ollama is STILL the embedder after scoring (not just before rebuild):
	// a daemon that dropped mid-scoring would silently mix static/ollama results.
	if m := chooseEmbedder().ModelID(); m != ollamaModel {
		t.Fatalf("A/B INVALID: Ollama embedder degraded to %q during scoring — the comparison would be mixed static/ollama", m)
	}
	if vecHits == 0 {
		t.Fatalf("A/B INVALID: the Ollama vec arm returned 0 hits across all queries despite %d stored vectors — the query embedder likely mismatched the index model, so the evaluated vec arm is empty", nVec)
	}

	t.Logf("=== static-vs-Ollama bucket migration (isolated copy of %s) ===", srcVault)
	t.Logf("static-hash-v1   : %s", fmtHist(staticHist))
	t.Logf("%-16s : %s  (%d vectors, %d vec-arm hits)", ollamaModel, fmtHist(ollamaHist), nVec, vecHits)
	t.Logf("verdict: RETRIEVAL %d→%d, HIT %d→%d  (RETRIEVAL↓ + HIT↑ ⇒ the embedder was the bottleneck; COVERAGE is embedder-invariant by construction)",
		staticHist[bRETRIEVAL], ollamaHist[bRETRIEVAL], staticHist[bHIT], ollamaHist[bHIT])
}
