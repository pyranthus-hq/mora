package mora

import (
	"context"
	"database/sql"
	"math"
	"strings"
)

// W2 / B1a — greedy Maximal Marginal Relevance rerank of the fused candidate pool.
//
// MMR reorders the post-fusion candidates to trade pure relevance against
// novelty, demoting a candidate that is redundant with one already chosen so the
// top-k the agent sees is less repetitive. It is a PURE PERMUTATION of the fused
// list (never adds, drops, or dedups), inserted between the fused sort and the
// top-k truncate in hybridSearchTrace, and is DEFAULT-OFF: production stays
// byte-identical to the pre-W2 fused order until a user opts in (Config.MMR) AND
// the vector arm is live (a semantic embedder — under the static-hash floor the
// stored vectors are lexical noise, the same reason useVec gates the vec arm).

// mmrParams configures the greedy MMR reranker. Like fusionParams it is an
// UNEXPORTED tuning/eval seam (see Config.mmrOv): NOT loaded from TOML. lambda
// trades relevance (1.0 ⇒ pure fused order) against diversity (0.0 ⇒ pure
// novelty); force is the EVAL-ONLY escape that runs MMR even under the
// static-hash floor (where useVec is false) so the CGO=0 CI eval can observe a
// rerank. Production NEVER sets force.
type mmrParams struct {
	lambda float64
	force  bool // eval seam ONLY: run MMR even when useVec=false. Never set in prod.
}

// defaultLambda is the canonical Carbonell-Goldstein relevance-leaning weight. On
// this scale (relevance min-maxed to [0,1], redundancy a clamped cosine in [0,1])
// a perfect duplicate costs (1-λ)=0.3, so a near-dup is demoted only when its
// normalized-relevance gap to a competitor is below 0.3/0.7≈0.43 — "demote the
// redundant, never leapfrog the clearly-more-relevant". It is the DEFAULT when the
// MMR opt-in is on, but is NOT a shippable default-on value: Config.MMR ships off
// and flipping it on by default is a separate decision gated on the Ollama A/B.
const defaultLambda = 0.7

// clampPos floors a value at 0. The repo's cosine (embed.go) is the dot product of
// L2-normalized SIGNED feature-hash vectors, so it ranges [-1,1] and is frequently
// negative for lexically-disjoint docs. An anti-similar pair is "not redundant", so
// its diversity penalty must be 0, NOT a spurious positive bonus (a raw negative
// cosine would flip -(1-λ)·sim into a reward for dissimilarity). Rescaling
// [-1,1]→[0,1] is wrong too: it would map cosine≈0 (unrelated) to 0.5 and penalize
// unrelated docs. Clamp-at-0 is the standard, correct redundancy semantics.
func clampPos(x float64) float64 {
	if x > 0 {
		return x
	}
	return 0
}

// mmrRerank returns a NEW permutation of ids by greedy MMR. It is a PURE
// permutation — never adds, drops, or dedups — so recall over the FULL list is
// invariant; only within-pool order (hence which docs survive the top-k truncate)
// changes. ids MUST enter in fused order (score desc, id asc); that order seeds the
// first pick and is the deterministic tie-break for equal MMR scores. Candidates
// absent from vecByID (no stored vector — e.g. a graph-only or FTS-only hit) are
// PINNED at their incoming fused index and never reordered, so MMR can neither give
// them a false novelty boost nor over-demote them, and a graph-only admission (the
// W1 killer feature) is never displaced out of the pool.
//
// rel maps id → fused RRF score; it is min-max normalized across the full pool to
// [0,1] so λ weighs relevance against the [0,1] redundancy term on one scale. The
// input ids slice and rel map are never mutated.
func mmrRerank(ids []string, rel map[string]float64, vecByID map[string][]float32, p mmrParams) []string {
	n := len(ids)
	if n <= 1 {
		return append([]string(nil), ids...)
	}

	// Partition FIRST: MMR reorders ONLY vector-backed candidates; missing-vec
	// candidates keep their fused slot. vecSlots are the indices (in fused order)
	// holding a vector-backed doc; the MMR order is written back into exactly those
	// slots. Partitioning before normalization is load-bearing: relNorm must be scoped
	// to the docs that actually compete, so a pinned (graph-only/FTS-only) outlier
	// fused score can't stretch the min-max span and silently rescale the pool's
	// effective λ (which would flip pool ordering — caught in adversarial review).
	out := append([]string(nil), ids...) // start = fused order; pinned docs stay put
	vecSlots := make([]int, 0, n)
	pool := make([]string, 0, n)
	for i, id := range ids {
		if _, ok := vecByID[id]; ok {
			vecSlots = append(vecSlots, i)
			pool = append(pool, id)
		}
	}
	if len(pool) < 2 {
		return out // nothing to diversify against
	}

	// relNorm: min-max across the POOL (the competing candidates); a degenerate
	// (all-equal) span → 1.0 for all, which keeps the fused-rank-0 doc as the seed and
	// orders the tail by novelty.
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, id := range pool {
		r := rel[id]
		if r < lo {
			lo = r
		}
		if r > hi {
			hi = r
		}
	}
	span := hi - lo
	relNorm := make(map[string]float64, len(pool))
	for _, id := range pool {
		if span <= 1e-12 {
			relNorm[id] = 1.0
		} else {
			relNorm[id] = (rel[id] - lo) / span
		}
	}

	selected := make([]string, 0, len(pool))
	remaining := append([]string(nil), pool...) // fused order
	for len(remaining) > 0 {
		bestIdx, bestScore := 0, math.Inf(-1)
		for j, c := range remaining { // remaining stays in fused order; strict > ⇒ lowest fused rank wins ties
			var maxSim float64
			for _, s := range selected {
				if sim := clampPos(cosine(vecByID[c], vecByID[s])); sim > maxSim {
					maxSim = sim
				}
			}
			score := p.lambda*relNorm[c] - (1-p.lambda)*maxSim
			if score > bestScore {
				bestScore, bestIdx = score, j
			}
		}
		selected = append(selected, remaining[bestIdx])
		remaining = append(remaining[:bestIdx], remaining[bestIdx+1:]...)
	}
	for k, slot := range vecSlots {
		out[slot] = selected[k]
	}
	return out
}

// mmrActive reports whether MMR should run given the live vector arm and the active
// params. The vector arm must be semantic (useVec) — under static-hash the vectors
// are lexical noise — OR the eval seam forces it. force can ONLY be set via the
// unexported mmrOv (the eval seam), never via the user MMR bool / TOML / env, so a
// production Config can never run MMR under static-hash: leak-proof by construction.
func mmrActive(useVec bool, p *mmrParams) bool {
	return useVec || p.force
}

// loadVectorsByID fetches stored vectors for the candidate ids under the active
// embedder's model. Ids without a row (graph-only / FTS-only hits, or a model
// mismatch) are simply absent from the map; the caller pins them. Bounded: ids is
// the fused pool (≤ ~50), so this is one IN(...) query.
func loadVectorsByID(ctx context.Context, db *sql.DB, model string, ids []string) (map[string][]float32, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ph := make([]string, len(ids))
	args := make([]any, 0, len(ids)+1)
	args = append(args, model)
	for i, id := range ids {
		ph[i] = "?"
		args = append(args, id)
	}
	rows, err := db.QueryContext(ctx,
		`SELECT memory_id, vec FROM mem_vectors WHERE model = ? AND memory_id IN (`+strings.Join(ph, ",")+`)`,
		args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string][]float32, len(ids))
	for rows.Next() {
		var id string
		var b []byte
		if err := rows.Scan(&id, &b); err != nil {
			return nil, err
		}
		out[id] = decodeVec(b)
	}
	return out, rows.Err()
}
