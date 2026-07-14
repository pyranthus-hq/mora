package mora

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pyranthus-hq/mora/internal/mora/exam"
	"pgregory.net/rapid"
)

// The round-trip property: for a GENERATED ledger, identity, artifact kind, block
// kind, commitment id, citation source and the exact authored-byte span all survive
// the trip through the real pipeline — exam.Render, then production's own
// parseMemory.
//
// It lives in package mora because the ingest leg is the point, and internal/mora/exam
// may not import internal/mora (that ban is what stops the scorer from reusing the
// implementation's cleaners and grading the engine against itself). The packet's
// dependency table says rapid may live only under internal/mora/exam; its acceptance
// criterion and Landmine 15 state the rule as a FILENAME rule (*_prop_test.go), which
// is what the AST guard can actually enforce, and which this file satisfies. The
// self-inconsistency was resolved by a coordinator gate: keep the real-ingest leg.
// rapid stays test-only either way — TestExamTestOnlyDepsAreNotLinked asserts it.

func TestPropRenderedLedgerRoundTripsThroughProduction(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		l := exam.GenerateLedger(func(label string, min, max int) int {
			return rapid.IntRange(min, max).Draw(rt, label)
		})
		files, err := exam.Render(l)
		if err != nil {
			rt.Fatalf("Render rejected a valid generated ledger: %v", err)
		}

		dir := t.TempDir()
		byMemory := map[string]Memory{}
		for rel, body := range files {
			path := filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(rel, "vault/")))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				rt.Fatal(err)
			}
			if err := os.WriteFile(path, body, 0o644); err != nil {
				rt.Fatal(err)
			}
			m, err := parseMemory(path)
			if err != nil {
				rt.Fatalf("production could not parse a rendered memory (%s): %v", rel, err)
			}
			// The renderer is pinned to memfile.go's own format, byte for byte, and
			// nothing but a byte comparison can prove it.
			back, err := renderMemory(m)
			if err != nil {
				rt.Fatal(err)
			}
			if string(back) != string(body) {
				rt.Fatalf("render -> parse -> render is not byte-identical for %s", rel)
			}
			byMemory[m.ID] = m
		}

		for _, a := range l.Artifacts {
			m, ok := byMemory[a.MemoryID]
			if !ok {
				rt.Fatalf("artifact %s did not survive as memory %q", a.ID, a.MemoryID)
			}
			if m.Title != a.Subject {
				rt.Errorf("artifact %s subject = %q after ingest, want %q", a.ID, m.Title, a.Subject)
			}
		}

		// The exact authored-byte span survives. This is the property the scorer's
		// EXACT match predicate rests on: if a gold quote could not be recovered
		// verbatim from the ingested memory, the only way to score anything would be
		// containment — and containment is what a copy-the-input extractor games.
		for _, c := range l.Commitments {
			assertSpanSurvives(rt, byMemory, l, c.ID, c.OpenedBy)
			for _, tr := range c.Transitions {
				assertSpanSurvives(rt, byMemory, l, c.ID, tr.Evidence)
			}
		}
		for _, n := range l.NonObligations {
			assertSpanSurvives(rt, byMemory, l, n.ID, n.Span)
		}
	})
}

func assertSpanSurvives(rt *rapid.T, byMemory map[string]Memory, l exam.Ledger, label string, span exam.Span) {
	for _, a := range l.Artifacts {
		if a.ID != span.ArtifactID {
			continue
		}
		m, ok := byMemory[a.MemoryID]
		if !ok {
			rt.Fatalf("%s cites artifact %s, whose memory never ingested", label, a.ID)
		}
		haystack := m.Text
		if span.MessageID == "" {
			haystack = m.Title
		}
		if !strings.Contains(haystack, span.Quote) {
			rt.Fatalf("%s: the gold span %q did not survive ingest into memory %q", label, span.Quote, a.MemoryID)
		}
		return
	}
	rt.Fatalf("%s cites unknown artifact %s", label, span.ArtifactID)
}
