package memory

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMemoryJSONContract(t *testing.T) {
	m := Memory{ID: "m1", Scope: "project:x", Type: "note", Title: "T", Tags: []string{"a"}, Source: "local", CreatedAt: "2026-01-01T00:00:00Z", Path: "/m.md", Decision: &DecisionValidity{AsOf: "2026-01-01T00:00:00Z", Durability: "working", FlipConditions: []string{"new evidence"}, Complete: true}, Corroborating: []CorroboratingRef{{ID: "m2", Title: "C", Source: "gmail", CreatedAt: "2026-01-01T00:00:00Z"}}, LaterRelatedEvidence: &LaterRelatedEvidence{ID: "m3", Title: "Later", Source: "calendar", IndexedAt: "2026-01-02T00:00:00Z"}, Evidence: &GmailSegmentEvidence{EvidenceRef: "m1#s1", Sender: "a@example.com", At: "2026-01-01T00:00:00Z", Snippet: "yes"}}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"id":"m1"`, `"decision":{`, `"corroborating":[`, `"later_related_evidence":{`, `"evidence":{`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("JSON %s missing %s", b, want)
		}
	}
	if strings.Contains(string(b), `"text"`) {
		t.Fatalf("omitempty text leaked: %s", b)
	}
}
func TestSourceIsEnabled(t *testing.T) {
	if (Source{}).IsEnabled() {
		t.Fatal("nil enabled must be false")
	}
	yes := true
	no := false
	if !(Source{Enabled: &yes}).IsEnabled() || (Source{Enabled: &no}).IsEnabled() {
		t.Fatal("explicit enablement mismatch")
	}
}
