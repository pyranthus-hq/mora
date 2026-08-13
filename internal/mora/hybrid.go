package mora

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"sort"
)

// rrfK is the Reciprocal Rank Fusion constant (k=60 is the standard from the
// original RRF paper). It dampens the head so a single list can't dominate.
const rrfK = 60.0

// fusionParams are the per-arm weights + the RRF damping constant used to fuse the
// FTS/vector/graph arms. They exist because equal-weight RRF at k=60 is too flat for
// this corpus: the gold doc is frequently retrieved by the FTS arm at a strong rank
// but then DEMOTED below the cutoff by docs that several arms weakly agree on (the
// FUSION-dominant failure mode measured in the live recall eval, where hybrid scored
// WORSE than the then-parent-FTS-only baseline). FTS is the exact-match correctness
// anchor and the strongest single arm, so it carries more weight, and a smaller k
// sharpens the head so a rank-0 hit isn't matched by a rank-50 also-ran. The
// vec/graph arms still ADD recall for docs FTS misses entirely. Tuned by
// TestEvalWeightSweep against the live golden set; see
// docs/design/2026-06-10-retrieval-ranking.md.
type fusionParams struct {
	fts, vec, graph, k float64
}

func (p fusionParams) weights() []float64 { return []float64{p.fts, p.vec, p.graph} }

// defaultFusion is the production fusion tuning, the SINGLE source of truth for the
// arm weights + damping so search, eval, and any config override agree. Tuned by
// TestEvalWeightSweep on an Ollama-indexed copy of the live golden set: the dominant
// lever was the damping k, NOT the weights — the standard k=60 is far too flat for a
// pool of ~50 (a rank-50 also-ran contributes ~as much as a rank-0 hit: 1/110 vs
// 1/61), which let weak cross-arm agreement DEMOTE strong FTS hits. Dropping to k=10
// sharpens the head and migrated FUSION→HIT. A GENTLE fts=1.5 anchors the
// exact-match arm for one more hit; heavier FTS (≥3) regressed (it buries the vector
// arm's vocabulary-mismatch rescues), so the weight stays light. See
// docs/design/2026-06-10-retrieval-ranking.md.
var defaultFusion = fusionParams{fts: 1.5, vec: 1, graph: 1, k: 10}

// rrfWeighted fuses ranked id lists into one score per id: score = Σ wᵢ/(k+rank).
// weights[i] multiplies list i's contribution; a missing/short weights slice (or a
// nil one) defaults that arm to weight 1.0, so equal/nil weights reduce to plain RRF.
// Rank-based (not score-based), so it fuses BM25's unbounded scores and cosine's
// [0,1] without normalization.
func rrfWeighted(lists [][]string, weights []float64, k float64) map[string]float64 {
	score := map[string]float64{}
	for li, list := range lists {
		w := 1.0
		if li < len(weights) {
			w = weights[li]
		}
		for rank, id := range list {
			score[id] += w / (k + float64(rank+1))
		}
	}
	return score
}

// rrf is plain (equal-weight) RRF — retained for callers/tests that don't tune arms.
func rrf(lists [][]string, k float64) map[string]float64 {
	return rrfWeighted(lists, nil, k)
}

// retrievalTrace exposes the per-arm ranked lists hybridSearch computes and
// otherwise discards, so the T2 eval can bucket every gold-doc miss as
// COVERAGE / RETRIEVAL / FUSION / HIT (see docs/design/2026-06-05-t2-recall-eval-design.md §6).
// It is populated on every call; production callers drop it.
type retrievalTrace struct {
	FTS, Vec, Graph, Segment, Fused []string // ranked ids; rank = slice index. Fused is the FULL ranking, pre-limit.
	// PreTruncPool is the arm depth actually queried. The eval passes a tracePool
	// LARGER than production's pool=limit*5 so a gold doc at arm-rank #55 surfaces
	// as FOUND-BUT-BEYOND-POOL (FUSION), not misread as "no arm found it"
	// (RETRIEVAL → falsely blames the embedder). Load-bearing for attribution.
	PreTruncPool int
}

// hybridSearch retrieves with FTS5/BM25 (the exact-match correctness anchor) +
// static-embedding cosine (recall) + 1-hop graph expansion (people the query
// names), plus Gmail segment-grain FTS, fused by RRF. It degrades gracefully:
// with no vector table (a pre-I2 index) it still runs parent FTS + Gmail segments;
// an empty query short-circuits to nil.
//
// It is a thin wrapper over hybridSearchTrace with tracePool=0, so the arm pool
// stays exactly limit*5 (min 50) and the fused ranking is byte-identical to the
// pre-trace implementation — one production code path, the trace discarded.
func hybridSearch(ctx context.Context, cfg Config, query, scope string, limit int, filters ...searchFilters) ([]Memory, error) {
	mems, _, err := hybridSearchTrace(ctx, cfg, query, scope, limit, 0, filters...)
	return mems, err
}

// embedderIsSemantic reports whether e produces real semantic vectors rather than
// the deterministic static-hash floor. Hybrid retrieval beat the measured
// parent-FTS baseline ONLY with a semantic embedder (T2 recall eval,
// docs/SESSION-2026-06-06-t2-eval.md); under
// static-hash hybrid REGRESSES recall (0.591 -> 0.394 @5), so the default search
// path gates on this.
func embedderIsSemantic(e Embedder) bool { return e.ModelID() != defaultEmbedder().ModelID() }

// mcpSearchResult is defaultSearch's routing decision + retrieval trace,
// exposed alongside its results for mcpSearchMemory's confidence envelope
// (#238 P1/P2 fix). Confidence must key its "scale"/strength on the SAME
// decision that actually routed and scored this call's results — never a
// second, independently-timed chooseEmbedderFor probe (which can disagree
// with the routing probe if Ollama reachability flips between the two) —
// and must reuse the SAME hybridSearchTrace pass rather than re-running the
// full embed+retrieve+fuse pipeline a second time just to recover the trace.
//
// Local/Trace are the PRE-union local results and arms. Trace is populated on
// the semantic path and on a static path whose Gmail segment arm participates;
// that lets confidence interpret the actual score domain rather than guessing
// from the embedder. Shared-corpus contributions remain outside the personal
// index's retrieval trace, mirroring buildThink's documented gap scope.
type mcpSearchResult struct {
	Results      []Memory       // post-union — the actual RETURNED set, identical to defaultSearch's return
	SemanticPath bool           // the SAME chooseEmbedderFor/embedderIsSemantic decision that routed retrieval
	ScoreFused   bool           // actual returned Memory.Score domain, including subscribed-share RRF
	Local        []Memory       // pre-union local results
	Trace        retrievalTrace // per-arm trace when ScoreFused is true
}

// defaultSearchForMCP is defaultSearch's routing + results, plus the ACTUAL
// routing decision and retrieval trace this call computed, threaded out for
// mcpSearchMemory's confidence envelope to consume (#238). defaultSearch
// itself delegates here so its OTHER call sites (the `mora search` CLI) keep
// their exact signature and behavior, unchanged and untouched.
//
// It routes to hybrid ONLY when the ACTIVELY CHOSEN embedder is semantic
// (Ollama opted in AND the daemon reachable); static-hash — including Ollama
// opted in but the daemon down — stays on the static keyword surface (parent
// FTS + Gmail segments). We gate on chooseEmbedder() rather than just
// MORA_EMBEDDER because a vector-empty hybrid is NOT equivalent to that static
// surface: the graph arm still shifts RRF ranking, so it would not match the
// measured baseline (codex review).
//
// HEALTH-12 / Packet D2 read path: DEGRADE VISIBLY. An unreachable configured
// `ollama` embedder makes chooseEmbedderFor err — route to the static keyword
// surface rather than hard-failing a search on a one-second daemon blip. The
// failure is disclosed by the reddened index health banner (indexHealthOf →
// degraded), not by a crash.
func defaultSearchForMCP(ctx context.Context, cfg Config, query, scope string, limit int, filters ...searchFilters) (mcpSearchResult, error) {
	var out mcpSearchResult
	var local []Memory
	var err error
	emb, embErr := chooseEmbedderFor(cfg)
	out.SemanticPath = embErr == nil && embedderIsSemantic(emb)
	if out.SemanticPath {
		out.ScoreFused = true
		local, out.Trace, err = hybridSearchTrace(ctx, cfg, query, scope, limit, 0, filters...)
	} else {
		var observed searchMemoryObservation
		local, err = searchMemoriesObserved(ctx, cfg, query, scope, limit, &observed, filters...)
		out.ScoreFused = observed.ScoreFused
		out.Trace = observed.Trace
	}
	if err != nil {
		return out, err
	}
	out.Local = local
	// Query-time union with subscribed share corpora (`mora share`): owner-
	// attributed, rank-fused, and a no-op returning `local` unchanged when no
	// subscriptions exist.
	var sharedFused bool
	out.Results, sharedFused, err = unionSharedResultsObserved(ctx, cfg, local, query, scope, limit, filters...)
	// The local retrieval paths annotate from their deeper pre-truncation pools.
	// Run the same conservative pass once over the final union so a newer related
	// row from a subscribed corpus (or the personal vault) can warn an older row
	// from the other side without erasing any deeper-pool hint already attached.
	out.Results = annotateLaterRelatedEvidence(out.Results, out.Results)
	out.ScoreFused = out.ScoreFused || sharedFused
	return out, err
}

// defaultSearch backs search_memory + `mora search`. See defaultSearchForMCP
// for the routing rule; this is a thin wrapper that discards the routing
// decision/trace so every OTHER call site keeps its exact pre-#238 signature
// and behavior — one production code path, the extras dropped.
func defaultSearch(ctx context.Context, cfg Config, query, scope string, limit int) ([]Memory, error) {
	res, err := defaultSearchForMCP(ctx, cfg, query, scope, limit)
	return res.Results, err
}

// hybridSearchTrace is hybridSearch with the per-arm ranked lists exposed for
// failure attribution. The fused production result is ALWAYS computed from arms
// queried at the production pool (limit*5, min 50) and fed to RRF whole, so it
// is byte-identical to the pre-trace hybridSearch regardless of tracePool.
//
// When tracePool > pool, the arm lists RECORDED IN THE TRACE are re-queried at
// the deeper tracePool, so a gold doc beyond the production pool surfaces as
// FOUND-BUT-BEYOND-POOL (FUSION) instead of being misread as "no arm found it"
// (RETRIEVAL → falsely blaming the embedder). tracePool<=0 (what hybridSearch
// passes) records the production arms themselves — one query per arm, zero extra
// work on the production hot path.
func hybridSearchTrace(ctx context.Context, cfg Config, query, scope string, limit, tracePool int, filters ...searchFilters) ([]Memory, retrievalTrace, error) {
	f := oneFilter(filters)
	var tr retrievalTrace
	if _, err := os.Stat(dbPath(cfg)); err != nil {
		if _, err := rebuildIndex(ctx, cfg); err != nil {
			return nil, tr, err
		}
	}
	db, err := openIndexRO(ctx, cfg)
	if err != nil {
		return nil, tr, err
	}
	defer db.Close()

	pool := limit * 5
	if pool < 50 {
		pool = 50
	}

	// Production arms — queried at `pool` and fed WHOLE to RRF, exactly as the
	// pre-trace hybridSearch did. The graph arm's per-person LIMIT is `pool` but
	// its deduped union across people may exceed pool; that whole union is fused,
	// never capped — capping it would change the fused ranking for multi-person
	// queries (and break byte-identity).
	ftsIDs, err := ftsSearchIDs(ctx, db, query, scope, pool, f)
	if err != nil {
		return nil, tr, err
	}
	vecOK := vectorsAvailable(ctx, db)
	var (
		emb              Embedder
		vecIDs, graphIDs []string
		useVec           bool
	)
	if vecOK {
		// ONE embedder per search: the production and deep-trace vector arms MUST
		// use the same model. Calling chooseEmbedder() twice would let a daemon that
		// drops between the calls query an ollama-keyed index with a static vector
		// (dim/model mismatch → empty arm), silently corrupting the trace.
		//
		// HEALTH-12 read path: an unreachable configured embedder makes this err;
		// leave emb nil and useVec false so the vector arm is simply skipped (FTS +
		// graph still answer) rather than hard-failing. Never substitute static here.
		resolved, embErr := chooseEmbedderFor(cfg)
		// The vector arm only EARNS a place in the fusion when the embedder is
		// SEMANTIC. Under the static-hash floor the stored vectors are deterministic
		// noise; fusing a noise arm via RRF rewards cross-arm coincidence and DEMOTES
		// strong single-arm (FTS) hits — the FUSION-dominant regression where hybrid
		// scored BELOW the then-parent-FTS-only baseline on the live recall eval (a
		// gold doc at FTS#0 fused to #15 because the noise vec arm ranked it #52).
		// Graph expansion is embedder-independent (pure metadata), so it always stays.
		if embErr == nil {
			emb = resolved
			useVec = embedderIsSemantic(emb)
		}
		if useVec {
			if vecIDs, err = vectorSearchIDs(ctx, db, emb, query, scope, pool, f); err != nil {
				return nil, tr, err
			}
		}
		if graphIDs, err = graphExpandIDs(ctx, db, query, scope, pool, f); err != nil {
			return nil, tr, err
		}
	}

	// Issue #243 production arm: bounded to the SAME parent pool as the other
	// candidate sources before either fusion or hydration. Best-effort by
	// design: a segment-query error degrades to an empty arm and no evidence.
	segIDs, gsegEvidence, segErr := gmailSegmentQueryArmBounded(ctx, db, query, scope, pool, f)
	if segErr != nil {
		segIDs, gsegEvidence = nil, nil
	}

	// Trace arms — deepened to tracePool for attribution ONLY (never fused). At
	// tracePool<=pool the production arms ARE the trace arms (no extra queries).
	if tracePool > pool {
		tr.PreTruncPool = tracePool
		if tr.FTS, err = ftsSearchIDs(ctx, db, query, scope, tracePool, f); err != nil {
			return nil, tr, err
		}
		if vecOK {
			if useVec {
				if tr.Vec, err = vectorSearchIDs(ctx, db, emb, query, scope, tracePool, f); err != nil {
					return nil, tr, err
				}
			}
			if tr.Graph, err = graphExpandIDs(ctx, db, query, scope, tracePool, f); err != nil {
				return nil, tr, err
			}
		}
		// Like the other trace arms, Segment is re-queried at tracePool only
		// for attribution. The production segIDs above remain the sole segment
		// list fed to RRF below.
		if traceSegment, _, traceErr := gmailSegmentQueryArmBounded(ctx, db, query, scope, tracePool, f); traceErr == nil {
			tr.Segment = traceSegment
		}
	} else {
		tr.PreTruncPool = pool
		tr.FTS, tr.Vec, tr.Graph, tr.Segment = ftsIDs, vecIDs, graphIDs, segIDs
	}

	// Issue #243 — segment-grain FTS as an ADDITIONAL candidate source, mapped
	// to PARENT ids, before fusion/slot accounting (frozen interface #3): one
	// more arm in the SAME RRF fusion every other arm feeds. A query with no
	// segment matches contributes an empty list, which is a complete no-op
	// for every OTHER id's fused score — the byte-identity guarantee for
	// non-participating memories holds automatically. Best-effort: a
	// segment-arm failure just means an empty arm, never a failed search.
	if len(ftsIDs) == 0 && len(vecIDs) == 0 && len(graphIDs) == 0 && len(segIDs) == 0 {
		return nil, tr, nil
	}

	fp := configFusion(cfg)
	fusionWeights := append(append([]float64{}, fp.weights()...), gmailSegmentArmWeight)
	fused := rrfWeighted([][]string{ftsIDs, vecIDs, graphIDs, segIDs}, fusionWeights, fp.k)
	ids := make([]string, 0, len(fused))
	for id := range fused {
		ids = append(ids, id)
	}
	// Deterministic order: fused score desc, then id asc (stable tie-break).
	sort.Slice(ids, func(i, j int) bool {
		if fused[ids[i]] != fused[ids[j]] {
			return fused[ids[i]] > fused[ids[j]]
		}
		return ids[i] < ids[j]
	})
	tr.Fused = append([]string(nil), ids...) // PURE fused ranking (pre-limit) for §6 attribution — MMR never touches it

	// W2/B1a: optional greedy MMR rerank of the fused pool before the top-k truncate.
	// Default-OFF (configMMR(cfg)==nil ⇒ skipped ⇒ byte-identical to the pre-W2 fused order).
	// Runs only when a semantic embedder is live (useVec) or the eval seam forces it;
	// emb is the same model the arms used (set in the vecOK block), avoiding a second
	// chooseEmbedderFor that could mismatch the index model. A pure permutation, so it
	// reorders which docs survive the truncate without changing the candidate set.
	// vecOK gates it: with no stored vectors (a pre-I2 index) there is nothing to
	// rerank on AND emb is unset, so MMR must no-op rather than deref a nil Embedder
	// (the force seam bypasses the useVec/semantic gate, never the vectors-exist gate).
	if mp := configMMR(cfg); mp != nil && vecOK && emb != nil && mmrActive(useVec, mp) && len(ids) > 1 {
		vecByID, err := loadVectorsByID(ctx, db, emb.ModelID(), ids)
		if err != nil {
			return nil, tr, err
		}
		ids = mmrRerank(ids, fused, vecByID, *mp)
	}

	// Issue #237 — corroborating-record clustering runs HERE: post-fusion,
	// pre-truncate. Hydrating the full fused ranking (not just the top `limit`)
	// is what makes the freed-slot backfill possible — a cluster's non-head
	// members give up their slots to the next-best DISTINCT candidates further
	// down `ids`, which requires seeing them before truncating.
	mems, err := loadMemoriesByID(ctx, cfg, db, ids)
	if err != nil {
		return nil, tr, err
	}
	// loadMemoriesByID returns newest-first AND already visibility-filtered
	// (suppressPendingDeletes/currentMemories, graph_read.go); re-order to the
	// fused ranking and stamp the fused score so callers/telemetry see the
	// retrieval signal. `ids` itself is the FULL fused order BEFORE that
	// visibility filtering ran — the legacy slot discipline's rawIDs window
	// (cluster.go's clusterAndTruncate doc comment).
	byID := make(map[string]Memory, len(mems))
	for _, m := range mems {
		byID[m.ID] = m
	}
	visible := make([]Memory, 0, len(ids))
	for _, id := range ids {
		if m, ok := byID[id]; ok {
			m.Score = fused[id]
			visible = append(visible, m)
		}
	}
	result := clusterAndTruncate(ids, visible, limit)
	// Issue #243 — attach evidence AFTER slot accounting: a pure function of
	// "does this SURVIVING row's parent have a query-matching segment" (DQ5),
	// independent of which arm(s) actually ranked it in.
	if len(segIDs) > 0 {
		gsegEvidence = completeGmailSegmentEvidence(ctx, db, query, scope, result, gsegEvidence, f)
	}
	attachGmailSegmentEvidence(result, gsegEvidence)
	return result, tr, nil
}

// vectorsAvailable reports whether the mem_vectors table exists and is populated.
func vectorsAvailable(ctx context.Context, db *sql.DB) bool {
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='mem_vectors'`).Scan(&n); err != nil || n == 0 {
		return false
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM mem_vectors`).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

// ftsSearchIDs returns up to pool memory ids ranked by BM25 for the query, scoped.
// An empty/punctuation-only query yields no ids (mirrors searchMemories).
//
// filters is an optional trailing #241 source/since_hours pair (zero value —
// a no-op, byte-identical query — when omitted). The filter is a TRUE SQL
// WHERE predicate (searchFilters.sqlPredicate) appended BEFORE ORDER
// BY/LIMIT, against the indexed memories.provider/account/created_at_unix
// columns — a filtered-out row is never fetched, so it can never crowd a
// matching row out of `pool` (filters_contract_test.go's
// TestFiltersHybridFTSArmPreRankProof).
func ftsSearchIDs(ctx context.Context, db *sql.DB, query, scope string, pool int, filters ...searchFilters) ([]string, error) {
	f := oneFilter(filters)
	match := ftsQuery(query)
	if match == "" {
		return nil, nil
	}
	q := `SELECT m.id FROM memories_fts JOIN memories m ON m.id = memories_fts.id WHERE memories_fts MATCH ?`
	args := []any{match}
	if scope != "" {
		q += ` AND m.scope = ?`
		args = append(args, scope)
	}
	if pc, pargs := f.sqlPredicate(); pc != "" {
		q += pc
		args = append(args, pargs...)
	}
	// Secondary sort by id makes ties deterministic — bm25 alone leaves equal-score
	// rows in undefined order, which would jitter the pool boundary run-to-run.
	q += ` ORDER BY bm25(memories_fts), m.id LIMIT ?`
	args = append(args, pool)
	return queryIDs(ctx, db, q, args...)
}

// vectorSearchIDs embeds the query and returns up to pool memory ids ranked by
// cosine similarity (brute force; <100ms to ~250k vectors). Zero-similarity rows
// are dropped so a query never pulls in wholly unrelated memories on vector alone.
//
// filters is an optional trailing #241 source/since_hours pair (zero value —
// a no-op — when omitted). The predicate is applied BEFORE the cosine
// computation for each row: a filtered-out row never earns a cosine score and
// so can never occupy a pool slot or influence ranking.
func vectorSearchIDs(ctx context.Context, db *sql.DB, emb Embedder, query, scope string, pool int, filters ...searchFilters) ([]string, error) {
	f := oneFilter(filters)
	qv, err := emb.Embed(query)
	if err != nil {
		return nil, err
	}
	q := `SELECT v.memory_id, v.vec FROM mem_vectors v JOIN memories m ON m.id = v.memory_id WHERE v.model = ?`
	args := []any{emb.ModelID()}
	if scope != "" {
		q += ` AND m.scope = ?`
		args = append(args, scope)
	}
	// #241: the SQL WHERE predicate excludes a filtered-out row from this
	// query's result set entirely — BEFORE the cosine loop below ever sees
	// it, satisfying "exclude before the cosine/top-k loop" by construction
	// rather than a Go-side per-row skip.
	if pc, pargs := f.sqlPredicate(); pc != "" {
		q += pc
		args = append(args, pargs...)
	}
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type scored struct {
		id  string
		sim float64
	}
	var cands []scored
	for rows.Next() {
		var id string
		var blob []byte
		if err := rows.Scan(&id, &blob); err != nil {
			return nil, err
		}
		if sim := cosine(qv, decodeVec(blob)); sim > 0 {
			cands = append(cands, scored{id, sim})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].sim != cands[j].sim {
			return cands[i].sim > cands[j].sim
		}
		return cands[i].id < cands[j].id // deterministic tie-break
	})
	out := make([]string, 0, min(pool, len(cands)))
	for i := 0; i < len(cands) && i < pool; i++ {
		out = append(out, cands[i].id)
	}
	return out, nil
}

// graphExpandIDs resolves the people named in the query (via the person gazetteer
// + exact alias match) and pulls their 1-hop evidence memories into the candidate
// pool — GraphRAG-lite, no LLM. Ordered newest-first for a stable rank.
//
// filters is an optional trailing #241 source/since_hours pair (zero value —
// a no-op, byte-identical query — when omitted). The filter is a TRUE SQL
// WHERE predicate (searchFilters.sqlPredicate) appended to EACH per-person
// query BEFORE its own ORDER BY/LIMIT — the per-person LIMIT is never
// dropped and there is no Go-side fallback/post-filter. A filtered-out row
// is excluded by the WHERE clause itself, so it is never fetched and can
// never crowd a matching row out of that person's `pool` slots.
func graphExpandIDs(ctx context.Context, db *sql.DB, query, scope string, pool int, filters ...searchFilters) ([]string, error) {
	f := oneFilter(filters)
	gaz, aliasToID, err := loadPersonGazetteer(ctx, db)
	if err != nil {
		return nil, err
	}
	matched := map[string]bool{}
	for _, id := range gazetteerScan(gaz, query) {
		matched[id] = true
	}
	// Exact alias/email/handle token match (precise queries like "neil@x.com").
	for _, tok := range tokenizeWords(query) {
		if id, ok := aliasToID[tok]; ok {
			matched[id] = true
		}
	}
	if len(matched) == 0 {
		return nil, nil
	}
	pids := make([]string, 0, len(matched))
	for id := range matched {
		pids = append(pids, id)
	}
	sort.Strings(pids)

	seen := map[string]bool{}
	var out []string
	for _, pid := range pids {
		q := `SELECT DISTINCT e.evidence_id FROM edges e JOIN memories m ON m.id = e.evidence_id
		      WHERE e.dst = ? AND e.invalidated_at IS NULL`
		args := []any{pid}
		if scope != "" {
			q += ` AND m.scope = ?`
			args = append(args, scope)
		}
		// #241: the SQL WHERE predicate (provider/account/created_at_unix)
		// excludes a filtered-out row from THIS per-person query's result set
		// entirely, before ORDER BY/LIMIT — never a Go-side post-filter.
		if pc, pargs := f.sqlPredicate(); pc != "" {
			q += pc
			args = append(args, pargs...)
		}
		q += ` ORDER BY m.created_at DESC, e.evidence_id ASC LIMIT ?`
		args = append(args, pool)
		ids, err := queryIDs(ctx, db, q, args...)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out, nil
}

// loadPersonGazetteer builds a query-time gazetteer (multi-token names) plus an
// exact alias→id map (emails/handles/single tokens) from the materialized person
// entities, so the same matching logic the indexer used applies to queries.
func loadPersonGazetteer(ctx context.Context, db *sql.DB) (gazetteer, map[string]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, display_name, aliases FROM entities WHERE id LIKE 'person:%'`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	g := gazetteer{}
	aliasToID := map[string]string{}
	for rows.Next() {
		var id, display, aliasesJSON string
		if err := rows.Scan(&id, &display, &aliasesJSON); err != nil {
			return nil, nil, err
		}
		consider := []string{display}
		var aliases []string
		if aliasesJSON != "" {
			_ = json.Unmarshal([]byte(aliasesJSON), &aliases)
		}
		consider = append(consider, aliases...)
		for _, a := range consider {
			if norm, ok := normalizeGazName(a); ok {
				if cur, exists := g[norm]; !exists || id < cur {
					g[norm] = id // deterministic: smallest id wins ambiguous names
				}
			}
			for _, tok := range tokenizeWords(a) {
				if _, exists := aliasToID[tok]; !exists {
					aliasToID[tok] = id
				}
			}
		}
	}
	return g, aliasToID, rows.Err()
}

// queryIDs runs a single-column id query and collects the results in order.
func queryIDs(ctx context.Context, db *sql.DB, q string, args ...any) ([]string, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
