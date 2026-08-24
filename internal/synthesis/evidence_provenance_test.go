package synthesis

import (
	"testing"

	"github.com/pyranthus-hq/mora/internal/memory"
)

func TestEvidenceFromMemoriesCarriesCanonicalProvenance(t *testing.T) {
	evidence := EvidenceFromMemories([]memory.Memory{{
		ID: "github_issue/o/r/1", Provider: "github", ProviderID: "o/r/1", CreatedAt: "2026-08-20T10:00:00Z",
		Meta: map[string]any{"occurred_at": "2026-08-19T09:00:00Z", "canonical_url": "https://github.com/o/r/issues/1"},
	}}, "issue")
	if len(evidence) != 1 || evidence[0].StableID == "" || evidence[0].CanonicalSourceID != "github" || evidence[0].Timestamp != "2026-08-19T09:00:00Z" || evidence[0].DeepLink != "https://github.com/o/r/issues/1" {
		t.Fatalf("evidence=%+v", evidence)
	}
}
