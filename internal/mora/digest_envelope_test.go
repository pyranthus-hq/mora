package mora

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustMarshalEnvelope(t *testing.T, env DigestEnvelope) string {
	t.Helper()
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return string(b)
}

// fixedSections is the canonical two-section / three-item digest used across the
// envelope tests: a known set of ids/titles/snippets so the prompt is fully
// pinned. Items are in the GIVEN order (the caller pre-sorts); the builder must
// preserve it.
func fixedSections() []DigestSection {
	return []DigestSection{
		{
			Source: "gmail",
			State:  "new",
			Items: []DigestItem{
				{ID: "gmail_thread/t01", Title: "Q3 planning sync", Source: "gmail", CreatedAt: "2026-06-07T10:00:00Z", Snippet: "agenda for the planning call", Change: "new"},
				{ID: "gmail_thread/t02", Title: "Invoice #4412", Source: "gmail", CreatedAt: "2026-06-07T11:00:00Z", Snippet: "payment due next week", Change: "updated"},
			},
		},
		{
			Source: "calendar",
			State:  "new",
			Items: []DigestItem{
				{ID: "gcal_event/e01", Title: "1:1 with Neil", Source: "calendar", CreatedAt: "2026-06-08T09:00:00Z", Snippet: "pilot feedback review", Change: "new"},
			},
		},
	}
}

// fixedStates is one healthy + one stale + one unavailable source — exercises the
// bounded NOT-COVERED line.
func fixedStates() []sourceState {
	return []sourceState{
		{Instance: "gmail", State: "new", Count: 2, LastSynced: "2026-06-08T08:00:00Z"},
		{Instance: "calendar", State: "stale", Count: 1, LastSynced: "2026-06-01T08:00:00Z"},
		{Instance: "imessage", State: "unavailable", Count: 0, Errored: true},
	}
}

func TestDigestEnvelopeJSONKeysMatchPayload(t *testing.T) {
	// The DigestEnvelope projection's JSON keys are a superset of digestMCPPayload's
	// base map keys plus synthesis_prompt — so the plain payload stays a strict
	// subset (D15-3 backward-compat). Marshal a zero value and assert the keys.
	env := DigestEnvelope{
		Generated:       "2026-06-08T00:00:00Z",
		SinceHours:      24,
		Sections:        fixedSections(),
		SourceStates:    fixedStates(),
		SynthesisPrompt: "x",
	}
	js := mustMarshalEnvelope(t, env)
	for _, key := range []string{
		`"generated"`, `"since_hours"`, `"sections"`, `"source_states"`, `"synthesis_prompt"`,
	} {
		if !strings.Contains(js, key) {
			t.Errorf("envelope JSON missing key %s:\n%s", key, js)
		}
	}
}
