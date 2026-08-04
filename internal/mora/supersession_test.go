package mora

import "testing"

func TestSupersessionHintFindsNewerRelatedEvidenceBeyondResultWindow(t *testing.T) {
	old := Memory{ID: "old", Scope: "project:mora", Title: "Issue 62 implementation not yet PR'd", Source: "mcp", CreatedAt: "2026-07-29T10:00:00Z"}
	newer := Memory{ID: "new", Scope: "project:mora", Title: "Issue 62 implementation merged", Source: "mcp", CreatedAt: "2026-07-30T10:00:00Z"}
	unrelated := Memory{ID: "other", Scope: "project:mora", Title: "Issue 62 launch party", Source: "mcp", CreatedAt: "2026-08-01T10:00:00Z"}

	got := annotateLaterRelatedEvidence([]Memory{old}, []Memory{old, unrelated, newer})
	if got[0].LaterRelatedEvidence == nil {
		t.Fatal("older result has no later-related evidence hint")
	}
	if ref := *got[0].LaterRelatedEvidence; ref.ID != newer.ID || ref.IndexedAt != newer.CreatedAt {
		t.Fatalf("later-related evidence = %+v, want newer record", ref)
	}
}

func TestSupersessionHintIsPrecisionFirstAndScopeBound(t *testing.T) {
	old := Memory{ID: "old", Scope: "project:mora", Title: "Invoice Neil", CreatedAt: "2026-06-01T10:00:00Z"}
	cases := []Memory{
		{ID: "one-token", Scope: "project:mora", Title: "Neil engagement ended", CreatedAt: "2026-07-01T10:00:00Z"},
		{ID: "other-scope", Scope: "project:other", Title: "Invoice Neil completed", CreatedAt: "2026-07-02T10:00:00Z"},
		{ID: "unknown-clock", Scope: "project:mora", Title: "Invoice Neil completed", Provider: "gmail", CreatedAt: "2026-07-03T10:00:00Z"},
	}
	got := annotateLaterRelatedEvidence([]Memory{old}, append([]Memory{old}, cases...))
	if got[0].LaterRelatedEvidence != nil {
		t.Fatalf("weak/cross-scope/unknown-clock candidate produced hint: %+v", got[0].LaterRelatedEvidence)
	}
}

func TestClusterAndTruncateCarriesSupersessionHintFromDeepPool(t *testing.T) {
	old := Memory{ID: "old", Scope: "global", Title: "OAuth rollout pending", CreatedAt: "2026-07-01T10:00:00Z"}
	newer := Memory{ID: "new", Scope: "global", Title: "OAuth rollout completed", CreatedAt: "2026-07-02T10:00:00Z"}
	got := clusterAndTruncate([]string{old.ID, newer.ID}, []Memory{old, newer}, 1)
	if len(got) != 1 || got[0].ID != old.ID {
		t.Fatalf("legacy rank/window changed: %+v", got)
	}
	if got[0].LaterRelatedEvidence == nil || got[0].LaterRelatedEvidence.ID != newer.ID {
		t.Fatalf("deep-pool newer record not surfaced: %+v", got[0].LaterRelatedEvidence)
	}
}
