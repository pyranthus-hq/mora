package graph

import (
	"testing"

	"github.com/pyranthus-hq/mora/internal/memory"
)

// Trust markers are not topics: neither the tentative tag nor an
// owner-confirmed tag becomes a tag entity.
func TestTrustTagsNeverBecomeEntities(t *testing.T) {
	m := memory.Memory{ID: "mem_1", Scope: "project:demo", Title: "t", Text: "x", Tags: []string{"pipeline", memory.TentativeTag}}
	c := memory.Memory{ID: "mem_2", Scope: "project:demo", Title: "t", Text: "y", Tags: []string{"pipeline"}}
	c.Tags = append(c.Tags, memory.OwnerConfirmedTagPrefix+memory.OwnerConfirmedDigest(c))
	var tags []string
	for _, e := range StructuralEntities([]memory.Memory{m, c}) {
		if e.Kind == "tag" {
			tags = append(tags, e.Name)
		}
	}
	if len(tags) != 1 || tags[0] != "pipeline" {
		t.Fatalf("tag entities = %v, want only pipeline", tags)
	}
}
