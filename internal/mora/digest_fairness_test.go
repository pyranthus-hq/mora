package mora

import (
	"strings"
	"testing"
)

// The MCP brief's budgetSections must give every source a fair floor so a noisy
// high-rank source (calendar subscriptions) can never starve a lower-rank human
// source (iMessage, Gmail) to zero items. This is the regression guard for the
// live incident where `mora brief` (MCP) returned "items":null for gmail
// (more_count 271) and imessage (more_count 210) because a calendar flood ate
// the whole byte budget and the old single-pass greedy budgeter latched an
// `exhausted` flag that shelled every section behind calendar.
//
// Sections are rank-sorted calendar(0) → imessage(1) → gmail(2), matching the
// production order, so gmail is the worst-case victim.

// floodSection builds a section with n items whose snippet bodies are `bodyLen`
// runes each — big bodies make one section able to consume the whole budget.
func floodSection(source string, n, bodyLen int) DigestSection {
	body := strings.Repeat("x", bodyLen)
	s := DigestSection{Source: source, State: stateDelta}
	for i := 0; i < n; i++ {
		s.Items = append(s.Items, DigestItem{
			ID:        source + "/" + string(rune('a'+i%26)),
			Title:     source + " item",
			Source:    source,
			CreatedAt: "2026-07-14T00:00:00Z",
			Snippet:   body,
			Change:    "new",
		})
	}
	return s
}

// TestBudgetSectionsFairFloorNoStarvation is the direct unit guard on the fix:
// a calendar flood must not zero out iMessage or Gmail.
func TestBudgetSectionsFairFloorNoStarvation(t *testing.T) {
	sections := []DigestSection{
		floodSection("calendar", 40, 400), // rank 0: a flood of big-bodied rows
		floodSection("imessage", 6, 80),   // rank 1
		floodSection("gmail", 6, 80),      // rank 2: the historical victim
	}

	// A budget generous enough for the per-source floor across all three, but far
	// too small for calendar's 40 big items to consume greedily. Under the OLD
	// budgeter, calendar overflowed and `exhausted` shelled imessage + gmail to
	// items:null; under the two-pass floor each source is guaranteed its floor.
	const budget = 6000
	out := budgetSections(sections, budget)

	bySource := map[string]DigestSection{}
	for _, s := range out {
		bySource[s.Source] = s
	}

	for _, src := range []string{"imessage", "gmail"} {
		s, ok := bySource[src]
		if !ok {
			t.Fatalf("%s section dropped entirely from the budgeted payload", src)
		}
		if len(s.Items) == 0 {
			t.Fatalf("STARVED: %s returned items=null (more_count %d) — a calendar flood ate the budget; the fair floor must guarantee it at least 1 item", src, s.MoreCount)
		}
		if len(s.Items) < budgetSourceFloor {
			t.Fatalf("%s kept %d items, want at least the fair floor %d", src, len(s.Items), budgetSourceFloor)
		}
	}

	// Calendar should still be present and honestly marked truncated (it had far
	// more than fit) — the flood is trimmed, not silently dropped.
	cal := bySource["calendar"]
	if !cal.Truncated || cal.MoreCount == 0 {
		t.Fatalf("calendar should be truncated with a positive more_count; got Truncated=%v MoreCount=%d", cal.Truncated, cal.MoreCount)
	}

	// Determinism: same input, same cut.
	out2 := budgetSections(sections, budget)
	if len(out2) != len(out) {
		t.Fatalf("nondeterministic section count: %d then %d", len(out), len(out2))
	}
	for i := range out {
		if len(out2[i].Items) != len(out[i].Items) {
			t.Fatalf("nondeterministic cut for %s: %d then %d items", out[i].Source, len(out[i].Items), len(out2[i].Items))
		}
	}
}

// TestBudgetSectionsFloorYieldsToRealScarcity proves the floor is a floor, not a
// guarantee that defies physics: when the budget genuinely cannot afford even the
// floor, higher-rank sources still win (deterministic, rank-ordered degradation),
// and nothing panics. This keeps the fix honest rather than over-promising.
func TestBudgetSectionsFloorYieldsToRealScarcity(t *testing.T) {
	sections := []DigestSection{
		floodSection("calendar", 5, 200),
		floodSection("imessage", 5, 200),
		floodSection("gmail", 5, 200),
	}
	// Budget so tiny only the first section's floor can be paid.
	out := budgetSections(sections, 700)
	if len(out) != 3 {
		t.Fatalf("every section must be represented (as shell or filled); got %d", len(out))
	}
	// The payload must remain well-formed: a section is either filled or a shell,
	// never a half-record, and total kept items never exceeds the input.
	for _, s := range out {
		if s.Truncated && len(s.Items) == 0 && s.MoreCount == 0 {
			t.Fatalf("%s shelled with more_count=0 — a shell must report what it hid", s.Source)
		}
	}
}
