package synthesis

import (
	"encoding/json"
	"github.com/pyranthus-hq/mora/internal/commitment"
	"github.com/pyranthus-hq/mora/internal/memory"
	"github.com/pyranthus-hq/mora/internal/openloops"
	"strings"
	"testing"
	"time"
)

func TestEvidenceFromMemories(t *testing.T) {
	text := strings.Repeat("opening ", 80) + "unique target here"
	mems := []memory.Memory{{ID: "id", Title: "title", Scope: "global", CreatedAt: "2026-01-01T00:00:00Z", Score: 1.5, Text: text, Provider: "gmail", Owner: "alice", Corroborating: []memory.CorroboratingRef{{ID: "c"}}}}
	got := EvidenceFromMemories(mems, "unique target")
	if len(got) != 1 || !strings.Contains(got[0].Snippet, "unique target") || got[0].Owner != "alice" {
		t.Fatalf("got=%+v", got)
	}
	title, full, source := got[0].ConfidenceFacts()
	if title != "title" || full != text || source != "gmail" {
		t.Fatalf("facts=%q %q %q", title, full, source)
	}
	b, err := json.Marshal(got[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "confidence") || !strings.Contains(string(b), `"corroborating"`) {
		t.Fatalf("json=%s", b)
	}
}
func TestEvidenceSourceFallback(t *testing.T) {
	mems := []memory.Memory{{ID: "source", Source: "s", Provider: "p", Type: "t"}, {ID: "provider", Provider: "p", Type: "t"}, {ID: "type", Type: "t"}}
	got := EvidenceFromMemories(mems, "")
	for i, want := range []string{"s", "p", "t"} {
		_, _, source := got[i].ConfidenceFacts()
		if source != want {
			t.Fatalf("%d source=%q", i, source)
		}
	}
}
func TestBasicGapsNoMatch(t *testing.T) {
	g := BasicGaps(nil, "q", time.Time{})
	if len(g.CoverageHoles) != 1 || len(g.ChecksApplied) != 6 || g.Empty() {
		t.Fatalf("gaps=%+v", g)
	}
}
func TestBasicGapsFreshSparseSingleSource(t *testing.T) {
	now := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	g := BasicGaps([]memory.Memory{{CreatedAt: now.Format(time.RFC3339), Source: "gmail"}}, "status", now)
	if len(g.SparseEvidence) != 1 || len(g.SourceCoverage) != 1 || len(g.Stale) != 0 || len(g.FreshnessUnknown) != 0 {
		t.Fatalf("gaps=%+v", g)
	}
}
func TestBasicGapsStaleUnknownAndMultiSource(t *testing.T) {
	now := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	stale := BasicGaps([]memory.Memory{{CreatedAt: now.Add(-31 * 24 * time.Hour).Format(time.RFC3339), Source: "gmail"}, {CreatedAt: "bad", Source: "calendar"}}, "q", now)
	if len(stale.Stale) != 1 || len(stale.SourceCoverage) != 0 {
		t.Fatalf("stale=%+v", stale)
	}
	unknown := BasicGaps([]memory.Memory{{CreatedAt: "bad", Source: "gmail"}, {CreatedAt: "also bad", Source: "calendar"}}, "q", now)
	if len(unknown.FreshnessUnknown) != 1 {
		t.Fatalf("unknown=%+v", unknown)
	}
}
func TestOutcomeAndProspectiveEvidence(t *testing.T) {
	if !OutcomeQuestion("How did the interview go?") || !OutcomeQuestion("What was the decision?") || OutcomeQuestion("When is the interview?") {
		t.Fatal("outcome classifier")
	}
	future := []memory.Memory{{Text: "interview scheduled"}, {Title: "calendar invitation confirmed"}}
	if !OnlyProspectiveEvidence(future) {
		t.Fatal("prospective evidence")
	}
	if OnlyProspectiveEvidence(append(future, memory.Memory{Text: "interview completed with an offer"})) || OnlyProspectiveEvidence([]memory.Memory{{Text: "unrelated"}}) {
		t.Fatal("outcome evidence")
	}
	g := BasicGaps(future, "What was the result?", time.Now())
	if len(g.TemporalState) != 1 {
		t.Fatalf("temporal=%+v", g)
	}
}
func TestEmpty(t *testing.T) {
	if !(Gaps{ChecksApplied: []string{"x"}}).Empty() {
		t.Fatal("checks are not gaps")
	}
	for _, g := range []Gaps{{Stale: []string{"x"}}, {FreshnessUnknown: []string{"x"}}, {SparseEvidence: []string{"x"}}, {SourceCoverage: []string{"x"}}, {TemporalState: []string{"x"}}, {ThinCoverage: []string{"x"}}, {CoverageHoles: []string{"x"}}, {RetrievalCaveats: []string{"x"}}} {
		if g.Empty() {
			t.Fatalf("reported empty: %+v", g)
		}
	}
}

func TestPromptNoEvidence(t *testing.T) {
	got := Prompt("Where?", nil, Gaps{}, nil)
	want := "Answer the question using ONLY the evidence below. Cite every claim with its [stable_id]. If the evidence is insufficient, say so plainly rather than guessing.\n\nQUESTION: Where?\n\nEVIDENCE:\n(none found)\n"
	if got != want {
		t.Fatalf("prompt=%q want %q", got, want)
	}
}
func TestPromptEvidenceGapsAndLoops(t *testing.T) {
	ev := []Evidence{{StableID: "local", Scope: "global", CreatedAt: "2026-01-01", Title: "Plan", Snippet: "body"}, {StableID: "shared", Owner: "alice", Scope: "team", CreatedAt: "2026-01-02", Title: "Note", Snippet: "shared body"}}
	gaps := Gaps{Stale: []string{"stale"}, FreshnessUnknown: []string{"unknown"}, SparseEvidence: []string{"sparse"}, SourceCoverage: []string{"source"}, TemporalState: []string{"temporal"}, ThinCoverage: []string{"thin"}, CoverageHoles: []string{"hole"}, RetrievalCaveats: []string{"caveat"}}
	loops := []openloops.Person{{Person: "Sam", Loops: []openloops.Loop{{Task: "send", Lifecycle: commitment.Open, Direction: commitment.OwedBySelf, Lane: openloops.LaneEvidence}}}}
	got := Prompt("Q", ev, gaps, loops)
	for _, want := range []string{"- [local] (global, 2026-01-01) Plan — body", "- [shared] (shared:alice, team, 2026-01-02) Note — shared body", "KNOWN GAPS", "- stale\n", "- unknown\n", "- sparse\n", "- source\n", "- temporal\n", "- thin\n", "- hole\n", "- caveat\n", "OPEN LOOPS", "Sam — send"} {
		if !strings.Contains(got, want) {
			t.Fatalf("prompt missing %q: %q", want, got)
		}
	}
}
