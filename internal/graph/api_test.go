package graph

import (
	"github.com/pyranthus-hq/mora/internal/memory"
	"testing"
)

func TestExportedCompatibilitySurface(t *testing.T) {
	m := memory.Memory{ID: "m1", Type: "email", Provider: "gmail", CreatedAt: "2026-01-01T00:00:00Z", Meta: map[string]any{"from": []string{"sam@example.com"}, "names": map[string]string{"sam@example.com": "Sam Rivera"}, "occurred_at": "2026-01-02T00:00:00Z"}}
	if ValidFrom(m) != "2026-01-02T00:00:00Z" || PersonID("sam@example.com") == "" || MailboxKey("First.Last+tag@googlemail.com") != "firstlast@gmail.com" {
		t.Fatal("identity adapters")
	}
	_ = MetaStrings(m.Meta["from"])
	_ = MetaNames(m.Meta["names"])
	_ = MetaPairs([]map[string]string{{"handle": "+1", "name": "Sam"}})
	_, _, _, _ = PersonRefs(m)
	_ = AggregatePersonSalience([]memory.Memory{m})
	if norm, ok := NormalizeGazName("Sam Rivera"); !ok || norm == "" {
		t.Fatal("normalize")
	}
	if got := ScanGazetteer(Gazetteer{"sam rivera": "person:sam"}, "Ask Sam Rivera"); len(got) != 1 {
		t.Fatalf("scan=%v", got)
	}
	_ = StructuralEntities([]memory.Memory{m})
	_ = TokenizeWords("one two")
	_ = PersonIdentity("person:sam@example.com")
	entities, edges, warnings := Compile([]memory.Memory{m})
	_ = entities
	_ = edges
	_ = warnings
	_ = Build([]memory.Memory{m}, []ConfirmedMerge{{A: "person:a", B: "person:b", GovID: "g1"}})
}
