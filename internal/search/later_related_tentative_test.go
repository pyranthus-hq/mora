package search

import (
	"testing"

	"github.com/pyranthus-hq/mora/internal/memory"
)

// A confirmed result must not point at a newer note nobody has confirmed: the
// pointer carries the note's title with no label.
func TestLaterRelatedSkipsTentativeCandidates(t *testing.T) {
	old := memory.Memory{ID: "old", Scope: "global", Title: "OAuth rollout pending", CreatedAt: "2026-07-01T10:00:00Z"}
	tent := memory.Memory{ID: "tent", Scope: "global", Title: "OAuth rollout completed", CreatedAt: "2026-07-02T10:00:00Z", Tags: []string{memory.TentativeTag}}
	got := AnnotateLaterRelatedEvidence([]memory.Memory{old}, []memory.Memory{old, tent})
	if got[0].LaterRelatedEvidence != nil {
		t.Fatalf("tentative note used as later-related evidence: %+v", got[0].LaterRelatedEvidence)
	}
}
