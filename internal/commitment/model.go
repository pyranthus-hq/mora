package commitment

import "github.com/pyranthus-hq/mora/internal/evidence"

const ClosureNone = "none"
const (
	CitationOpener     = "opener"
	CitationClosure    = "closure"
	CitationSupporting = "supporting"
)

type Citation struct {
	Citation     evidence.Citation `json:"citation"`
	CommitmentID string            `json:"commitment_id,omitempty"`
	Role         string            `json:"role"`
	EvidenceRef  string            `json:"evidence_ref,omitempty"`
}
type Record = Item
