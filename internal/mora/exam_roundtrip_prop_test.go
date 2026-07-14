package mora

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pyranthus-hq/mora/internal/mora/exam"
	"pgregory.net/rapid"
)

// The round-trip property: for a GENERATED ledger, identity, artifact kind,
// citation source and the exact authored-byte span all survive the trip through
// the real pipeline — exam.Render, then production's own parseMemory.
//
// Block ids/kinds and commitment ids are ledger-only scorer provenance: the Mora
// memory format does not serialize them, so this property deliberately does not
// claim they emerge from parseMemory. The citation-source assertion below instead
// pins every ledger span to the exact parsed memory that its artifact names.
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
			assertArtifactContractSurvives(rt, l, a, m)
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

func assertArtifactContractSurvives(rt *rapid.T, l exam.Ledger, a exam.Artifact, m Memory) {
	want := map[string][3]string{
		"gmail":    {"email", "gmail", strings.TrimPrefix(a.MemoryID, "gmail_thread/")},
		"calendar": {"event", "calendar", strings.TrimPrefix(a.MemoryID, "calendar_event/")},
		"imessage": {"imessage", "imessage", strings.TrimPrefix(a.MemoryID, "imessage_chat/")},
		"notes":    {"note", "", "manual"},
	}[a.Channel]
	wantType, wantProvider, wantSource := want[0], want[1], want[2]
	if m.Type != wantType || m.Provider != wantProvider || m.Source != wantSource {
		rt.Fatalf("artifact %s kind/source = type:%q provider:%q source:%q, want type:%q provider:%q source:%q",
			a.ID, m.Type, m.Provider, m.Source, wantType, wantProvider, wantSource)
	}

	identities := map[string]exam.Identity{l.Self.ID: l.Self}
	for _, p := range l.People {
		identities[p.ID] = p
	}
	wantIDs := map[string]bool{}
	switch a.Channel {
	case "gmail", "calendar":
		for _, msg := range a.Messages {
			wantIDs[msg.From] = true
			for _, id := range append(append([]string(nil), msg.To...), msg.Cc...) {
				wantIDs[id] = true
			}
		}
	case "imessage":
		for _, id := range a.Participants {
			wantIDs[id] = true
		}
	}
	meta, err := json.Marshal(m.Meta)
	if err != nil {
		rt.Fatal(err)
	}
	identityBytes := strings.ToLower(string(meta) + "\n" + m.Text)
	for id := range wantIDs {
		identity := identities[id]
		var stable string
		if a.Channel == "imessage" {
			if len(identity.Handles) > 0 {
				stable = identity.Handles[0]
			}
		} else if len(identity.Emails) > 0 {
			stable = identity.Emails[0]
		}
		if stable != "" && !strings.Contains(identityBytes, strings.ToLower(stable)) {
			rt.Fatalf("artifact %s lost stable identity %q for %s", a.ID, stable, id)
		}
		if identity.Display != "" && !strings.Contains(identityBytes, strings.ToLower(identity.Display)) {
			rt.Fatalf("artifact %s lost display identity %q for %s", a.ID, identity.Display, id)
		}
	}
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
		if m.ID != a.MemoryID {
			rt.Fatalf("%s citation source resolved to memory %q, want %q", label, m.ID, a.MemoryID)
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
