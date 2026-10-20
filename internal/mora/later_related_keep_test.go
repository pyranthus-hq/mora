package mora

import (
	"testing"

	"github.com/pyranthus-hq/mora/internal/memory"
)

// A pointer attached from the deeper search pool must survive the shallower
// context pass, which would otherwise replace a March pointer with a February
// one because March never made it into the returned set.
func TestAnnotateLaterRelatedKeepsAnExistingPointer(t *testing.T) {
	jan := Memory{ID: "mem_jan", Scope: "global", Title: "Widget pilot", CreatedAt: "2026-01-01T00:00:00Z",
		LaterRelatedEvidence: &memory.LaterRelatedEvidence{ID: "mem_mar", Title: "Widget pilot"}}
	feb := Memory{ID: "mem_feb", Scope: "global", Title: "Widget pilot", CreatedAt: "2026-02-01T00:00:00Z"}
	got := annotateLaterRelated([]Memory{jan, feb})
	if got[0].LaterRelatedEvidence == nil || got[0].LaterRelatedEvidence.ID != "mem_mar" {
		t.Fatalf("the deeper pointer was replaced: %+v", got[0].LaterRelatedEvidence)
	}
	if got[1].LaterRelatedEvidence != nil {
		t.Fatalf("february has nothing newer in the set, got %+v", got[1].LaterRelatedEvidence)
	}
	bare := annotateLaterRelated([]Memory{{ID: "mem_a", Scope: "global", Title: "Widget pilot", CreatedAt: "2026-01-01T00:00:00Z"},
		{ID: "mem_b", Scope: "global", Title: "Widget pilot", CreatedAt: "2026-02-01T00:00:00Z"}})
	if bare[0].LaterRelatedEvidence == nil || bare[0].LaterRelatedEvidence.ID != "mem_b" {
		t.Fatalf("a row with no pointer should gain one: %+v", bare[0].LaterRelatedEvidence)
	}
}
