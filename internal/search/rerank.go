package search

import (
	"sort"
	"strings"

	"github.com/pyranthus-hq/mora/internal/memory"
)

// Rerank bonuses, in rank positions. A bonus is how many places a row may move
// up, and no bonus is ever negative, so a row moves up at most its own bonus.
// A row never gains from its score, only from what the record is.
const (
	rerankCorrection     = 4 // a written correction
	rerankDecision       = 3 // a written decision
	rerankNote           = 1 // a written fact or insight
	rerankAuthored       = 1 // an authored memory (mem_*) vs ingested source
	rerankOwnerConfirmed = 2 // the owner kept this exact text in `mora review`
)

// rerankBonus is the row's promotion in positions. A tentative note gets 0:
// an agent chooses its own type, so an unreviewed claim must not rise above
// evidence by naming itself a decision. An owner-confirmed note gets its type
// bonus plus 2. Any other written note gets its type bonus, plus 1 when it is
// an authored memory (mem_*). A source file or connector record gets 0,
// because its type says what the file is, not what anyone decided.
func rerankBonus(m memory.Memory) int {
	if memory.IsTentative(m) || memory.DeriveProvenance(m) != memory.ProvenanceAuthored {
		return 0
	}
	if strings.HasPrefix(m.ID, "src_") {
		return 0
	}
	bonus := 0
	switch strings.ToLower(m.Type) {
	case "correction":
		bonus = rerankCorrection
	case "decision":
		bonus = rerankDecision
	case "fact", "insight":
		bonus = rerankNote
	}
	if strings.HasPrefix(m.ID, "mem_") {
		bonus += rerankAuthored
	}
	if memory.IsOwnerConfirmed(m) {
		bonus += rerankOwnerConfirmed
	}
	return bonus
}

// RerankByMetadata returns the rows in a new order. It adds, drops and changes
// no row and never touches Score. Each row sorts by position minus its
// bonus, so a row moves up at most its own bonus. Ties keep the original order,
// except that a promoted row goes first. The result is a pure function of the input order and metadata.
func RerankByMetadata(rows []memory.Memory) []memory.Memory {
	out := append([]memory.Memory(nil), rows...)
	if len(out) < 2 {
		return out
	}
	type keyed struct {
		pos, key int
		promoted bool
		row      memory.Memory
	}
	ks := make([]keyed, len(out))
	for i, m := range out {
		bonus := rerankBonus(m)
		ks[i] = keyed{pos: i, key: i - bonus, promoted: bonus > 0, row: m}
	}
	sort.Slice(ks, func(a, b int) bool {
		x, y := ks[a], ks[b]
		if x.key != y.key {
			return x.key < y.key
		}
		if x.promoted != y.promoted {
			return x.promoted
		}
		return x.pos < y.pos
	})
	for i, k := range ks {
		out[i] = k.row
	}
	return out
}

// RerankPool reranks the visible candidates and keeps rawIDs true for
// ClusterAndTruncate's slot rule. rawIDs is the pre-filter ranking and its
// first `limit` ids are the window. A row that filtering removed keeps its
// place in rawIDs and still costs its slot; the visible rows fill the other
// places in their new order, so visible stays a subsequence of rawIDs.
func RerankPool(rawIDs []string, visible []memory.Memory) ([]string, []memory.Memory) {
	ranked := RerankByMetadata(visible)
	isVisible := make(map[string]bool, len(visible))
	for _, m := range visible {
		isVisible[m.ID] = true
	}
	next := make([]string, len(rawIDs))
	k := 0
	for i, id := range rawIDs {
		if isVisible[id] && k < len(ranked) {
			next[i] = ranked[k].ID
			k++
		} else {
			next[i] = id
		}
	}
	if k != len(ranked) {
		return rawIDs, ranked
	}
	return next, ranked
}
