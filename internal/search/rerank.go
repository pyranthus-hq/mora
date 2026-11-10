package search

import (
	"sort"
	"strings"

	"github.com/pyranthus-hq/mora/internal/memory"
)

// Rerank bonuses, in rank positions. A bonus is how many places a row may move
// up. A row never gains from its score, only from what the record is.
const (
	rerankCorrection     = 4 // a written correction
	rerankDecision       = 3 // a written decision
	rerankNote           = 1 // a written fact or insight
	rerankOwnerConfirmed = 2 // the owner kept this exact text in `mora review`
	rerankTentative      = 2 // nobody confirmed this agent write: others may pass it
)

// rerankNet is the row's promotion in positions: type bonus plus trust bonus,
// minus the tentative penalty. A source file or connector record has no type
// bonus, because its type says what the file is, not what anyone decided.
func rerankNet(m memory.Memory) int {
	net := 0
	if memory.DeriveProvenance(m) == memory.ProvenanceAuthored {
		switch strings.ToLower(m.Type) {
		case "correction":
			net = rerankCorrection
		case "decision":
			net = rerankDecision
		case "fact", "insight":
			net = rerankNote
		}
	}
	if memory.IsOwnerConfirmed(m) {
		net += rerankOwnerConfirmed
	} else if memory.IsTentative(m) {
		net -= rerankTentative
	}
	return net
}

// RerankByMetadata returns the rows in a new order. It adds, drops and changes
// no row and never touches Score. Each row sorts by position minus its net
// bonus, so a row moves up about as far as its bonus and a tentative row only
// lets others pass it. Ties keep the original order, except that a promoted row
// goes first. The result is a pure function of the input order and metadata.
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
		net := rerankNet(m)
		ks[i] = keyed{pos: i, key: i - net, promoted: net > 0, row: m}
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
