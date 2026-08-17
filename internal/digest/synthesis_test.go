package digest

import (
	"strings"
	"testing"
)

func fixedSections() []Section {
	return []Section{
		{
			Source: "gmail",
			State:  "new",
			Items: []Item{
				{ID: "gmail_thread/t01", Title: "Q3 planning sync", Source: "gmail", CreatedAt: "2026-06-07T10:00:00Z", Snippet: "agenda for the planning call", Change: "new"},
				{ID: "gmail_thread/t02", Title: "Invoice #4412", Source: "gmail", CreatedAt: "2026-06-07T11:00:00Z", Snippet: "payment due next week", Change: "updated"},
			},
		},
		{
			Source: "calendar",
			State:  "new",
			Items: []Item{
				{ID: "gcal_event/e01", Title: "1:1 with Neil", Source: "calendar", CreatedAt: "2026-06-08T09:00:00Z", Snippet: "pilot feedback review", Change: "new"},
			},
		},
	}
}

func fixedStates() []SourceState {
	return []SourceState{
		{Instance: "gmail", State: "new"},
		{Instance: "calendar", State: "stale"},
		{Instance: "imessage", State: "unavailable"},
	}
}

func TestDigestSynthesisPromptCitesEveryItem(t *testing.T) {
	out := SynthesisPrompt(nil, fixedSections(), fixedStates())

	// Every item id is cited as a [id] token, and every title appears.
	for _, want := range []string{
		"[gmail_thread/t01]", "[gmail_thread/t02]", "[gcal_event/e01]",
		"Q3 planning sync", "Invoice #4412", "1:1 with Neil",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("prompt missing %q\n---\n%s", want, out)
		}
	}
}

func TestDigestSynthesisPromptGroundingInstruction(t *testing.T) {
	out := SynthesisPrompt(nil, fixedSections(), fixedStates())

	// The four grounding substrings that make the contract unambiguous (D15-5).
	for _, want := range []string{
		"ONLY",
		"cite each claim by its [id]",
		"read_memory",
		"do not invent",
	} {
		if !strings.Contains(strings.ToLower(out), strings.ToLower(want)) {
			t.Errorf("grounding instruction missing %q\n---\n%s", want, out)
		}
	}
}

func TestDigestSynthesisPromptItemOrderPreserved(t *testing.T) {
	out := SynthesisPrompt(nil, fixedSections(), fixedStates())
	// Items must appear in the GIVEN order (caller pre-sorted) — the builder does
	// not re-sort, so the prompt order matches the emitted items.
	i01 := strings.Index(out, "[gmail_thread/t01]")
	i02 := strings.Index(out, "[gmail_thread/t02]")
	iE01 := strings.Index(out, "[gcal_event/e01]")
	if i01 >= i02 || i02 >= iE01 {
		t.Errorf("item order not preserved: t01=%d t02=%d e01=%d", i01, i02, iE01)
	}
}

func TestDigestSynthesisPromptNoDanglingCitation(t *testing.T) {
	// The caller passes already-budgeted/truncated sections. A section truncated to
	// 2 of 5 items must yield a prompt citing exactly those 2 ids — never the 3
	// dropped ones (the no-dangling grounding invariant, T-15-01).
	truncated := []Section{
		{
			Source:    "gmail",
			State:     "new",
			Truncated: true,
			MoreCount: 3,
			Items: []Item{
				{ID: "gmail_thread/keep1", Title: "Kept one", Source: "gmail", Snippet: "a"},
				{ID: "gmail_thread/keep2", Title: "Kept two", Source: "gmail", Snippet: "b"},
			},
		},
	}
	out := SynthesisPrompt(nil, truncated, nil)

	for _, want := range []string{"[gmail_thread/keep1]", "[gmail_thread/keep2]"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected kept id %q in prompt\n---\n%s", want, out)
		}
	}
	for _, dropped := range []string{"gmail_thread/drop3", "gmail_thread/drop4", "gmail_thread/drop5"} {
		if strings.Contains(out, dropped) {
			t.Errorf("dropped id %q must NOT appear (dangling citation)\n---\n%s", dropped, out)
		}
	}
}

func TestDigestSynthesisPromptNotCoveredLinePresent(t *testing.T) {
	out := SynthesisPrompt(nil, fixedSections(), fixedStates())

	// The bounded NOT-COVERED line names the stale + unavailable instances.
	low := strings.ToLower(out)
	if !strings.Contains(low, "does not cover") {
		t.Errorf("expected NOT-COVERED line, got:\n%s", out)
	}
	if !strings.Contains(out, "calendar") || !strings.Contains(out, "imessage") {
		t.Errorf("NOT-COVERED line must name the stale (calendar) + unavailable (imessage) instances:\n%s", out)
	}
	// The healthy "new" instance must NOT be listed as uncovered. Locate the line
	// and assert gmail is absent from it.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(strings.ToLower(line), "does not cover") {
			if strings.Contains(line, "gmail") {
				t.Errorf("healthy instance gmail must not appear in NOT-COVERED line: %q", line)
			}
		}
	}
}

func TestDigestSynthesisPromptNotCoveredLineAbsentWhenHealthy(t *testing.T) {
	healthy := []SourceState{
		{Instance: "gmail", State: "new"},
		{Instance: "calendar", State: "no_change"},
	}
	out := SynthesisPrompt(nil, fixedSections(), healthy)
	if strings.Contains(strings.ToLower(out), "does not cover") {
		t.Errorf("no stale/unavailable source — NOT-COVERED line must be absent:\n%s", out)
	}
}

func TestDigestSynthesisPromptNotCoveredInstancesSorted(t *testing.T) {
	// Determinism: the uncovered instances are sorted, regardless of input order.
	states := []SourceState{
		{Instance: "zebra", State: "stale"},
		{Instance: "alpha", State: "unavailable"},
		{Instance: "mango", State: "stale"},
	}
	out := SynthesisPrompt(nil, fixedSections(), states)
	ia := strings.Index(out, "alpha")
	im := strings.Index(out, "mango")
	iz := strings.Index(out, "zebra")
	if ia < 0 || im < 0 || iz < 0 || ia >= im || im >= iz {
		t.Errorf("uncovered instances not sorted: alpha=%d mango=%d zebra=%d", ia, im, iz)
	}
}

func TestDigestSynthesisPromptEmptyInput(t *testing.T) {
	out := SynthesisPrompt(nil, nil, nil)
	if out == "" {
		t.Fatal("empty input must still produce a defined prompt")
	}
	if !strings.Contains(out, "(no items)") {
		t.Errorf("empty input must carry the (no items) marker:\n%s", out)
	}
	// The grounding instruction must still be present even with no items.
	if !strings.Contains(strings.ToLower(out), "do not invent") {
		t.Errorf("grounding instruction missing on empty input:\n%s", out)
	}
	// No stale/unavailable states → no NOT-COVERED line.
	if strings.Contains(strings.ToLower(out), "does not cover") {
		t.Errorf("empty states must not produce a NOT-COVERED line:\n%s", out)
	}
}

func TestDigestSynthesisPromptDeterministic(t *testing.T) {
	a := SynthesisPrompt(nil, fixedSections(), fixedStates())
	b := SynthesisPrompt(nil, fixedSections(), fixedStates())
	if a != b {
		t.Errorf("prompt not byte-identical across two calls:\n--- a ---\n%s\n--- b ---\n%s", a, b)
	}
}

func TestDigestSynthesisPromptGrowsWithItemCount(t *testing.T) {
	// Boundedness sanity: the prompt is a fixed template + one bounded line per
	// item, so more items → a longer prompt (lets 15-02 reserve space predictably).
	small := SynthesisPrompt(nil, fixedSections()[:1], nil)
	big := SynthesisPrompt(nil, fixedSections(), nil)
	if len(big) <= len(small) {
		t.Errorf("prompt should grow with item count: small=%d big=%d", len(small), len(big))
	}
}

func TestSynthesisPromptUrgentItemLeads(t *testing.T) {
	out := SynthesisPrompt([]Item{{ID: "urgent/1", Source: "gmail", Title: "Sign today", Snippet: "due by EOD"}}, fixedSections()[:1], nil)
	urgent := strings.Index(out, "[urgent/1]")
	normal := strings.Index(out, "[gmail_thread/t01]")
	if urgent < 0 || normal < 0 || urgent >= normal || !strings.Contains(out, "⚠ urgent") {
		t.Fatalf("urgent item did not lead: %s", out)
	}
}
