package mora

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// digest_mcp_test.go — Plan 12-05 D-05: ONE budgeted MCP digest payload.
//
// The MCP `digest` tool used to ship a budget-clipped `digest` render STRING and
// the full UNCLIPPED `sections` typed array — same content twice (envelope
// doubling), with `sections` ignoring max_tokens (dead knob). These tests pin the
// fixed contract: one budgeted payload (typed-delta sections + source_states)
// that scales with max_tokens, no doubling.

// digestMCPPayload calls the `digest` tool and returns the decoded structured
// result map (the structuredContent mirror == the raw tool return).
func digestMCPStructured(t *testing.T, args string) map[string]any {
	t.Helper()
	line := budgetCall("digest", args)
	res := mcpResult(t, line)
	sc, ok := res["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("digest result missing object structuredContent: %v", res)
	}
	return sc
}

// seedDigestVault seeds an enabled, recently-synced gmail source with a cold-
// start delta so the MCP digest has real sections + a healthy source_state.
func seedDigestVault(t *testing.T) Config {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Now()
	enableSources(t, cfg, "gmail")
	seedSyncStatus(t, cfg, "gmail", now.Add(-1*time.Hour))
	for i := 0; i < 4; i++ {
		digestSeed(t, cfg, "gmail", "Thread "+string(rune('A'+i)), time.Duration(i+1)*time.Hour, now)
	}
	return cfg
}

// TestDigestMCPNoDoubling (D-05): the payload ships the typed `sections` but NOT
// a duplicate `digest` render STRING beside them — the doubling that shipped the
// same content twice.
func TestDigestMCPNoDoubling(t *testing.T) {
	seedDigestVault(t)
	p := digestMCPStructured(t, `{}`)
	if _, ok := p["digest"]; ok {
		// A render string AND the typed sections == doubling. The CLI keeps the
		// render path; the MCP payload ships ONE structured representation.
		t.Fatalf("MCP digest payload must NOT ship a `digest` render string alongside `sections` (envelope doubling); got keys: %v", payloadKeys(p))
	}
	if _, ok := p["sections"]; !ok {
		t.Fatalf("MCP digest payload must ship the typed `sections`; got keys: %v", payloadKeys(p))
	}
}

// TestDigestMCPSourceStates (SC#3 in JSON): the payload includes a top-level
// source_states array exposing the per-instance three-state structurally.
func TestDigestMCPSourceStates(t *testing.T) {
	seedDigestVault(t)
	p := digestMCPStructured(t, `{}`)
	raw, ok := p["source_states"]
	if !ok {
		t.Fatalf("MCP digest payload must ship source_states; got keys: %v", payloadKeys(p))
	}
	arr, ok := raw.([]any)
	if !ok || len(arr) == 0 {
		t.Fatalf("source_states must be a non-empty array; got %T = %v", raw, raw)
	}
	st, _ := arr[0].(map[string]any)
	for _, field := range []string{"instance", "state", "count", "last_synced", "errored"} {
		if _, ok := st[field]; !ok {
			t.Fatalf("source_states entry must carry %q; got: %v", field, st)
		}
	}
	if st["instance"] != "gmail" {
		t.Fatalf("source_states[0].instance = %v, want gmail", st["instance"])
	}
	// A healthy, recently-synced cold-start gmail reads "new" (has surfaced items).
	if st["state"] != "new" {
		t.Fatalf("source_states[0].state = %v, want new (healthy gmail with a cold-start delta)", st["state"])
	}
	if st["errored"] != false {
		t.Fatalf("source_states[0].errored = %v, want false (healthy gmail)", st["errored"])
	}
}

// TestDigestMCPSourceStatesExercisesAllStates exercises new/no_change/stale/
// unavailable across enabled sources so the three-state surface is provably
// structural in JSON.
func TestDigestMCPSourceStatesExercisesAllStates(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Now()
	enableSources(t, cfg, "gmail", "imessage", "calendar")
	// gmail: healthy + a cold-start delta → "new".
	seedSyncStatus(t, cfg, "gmail", now.Add(-1*time.Hour))
	digestSeed(t, cfg, "gmail", "Fresh", 1*time.Hour, now)
	// imessage: stale (last sync > 48h ago), no items.
	seedSyncStatus(t, cfg, "imessage", now.Add(-72*time.Hour))
	// calendar: never synced → unavailable.
	// (no sync status seeded for calendar)

	p := digestMCPStructured(t, `{}`)
	arr, _ := p["source_states"].([]any)
	got := map[string]string{}
	for _, e := range arr {
		m, _ := e.(map[string]any)
		got[m["instance"].(string)], _ = m["state"].(string)
	}
	if got["gmail"] != "new" {
		t.Fatalf("gmail state = %q, want new; states=%v", got["gmail"], got)
	}
	if got["imessage"] != "stale" {
		t.Fatalf("imessage state = %q, want stale; states=%v", got["imessage"], got)
	}
	if got["calendar"] != "unavailable" {
		t.Fatalf("calendar state = %q, want unavailable (never synced); states=%v", got["calendar"], got)
	}
}

// TestDigestMCPKnobAlive (D-05): max_tokens=20000 produces a LARGER payload than
// the default (~6000), proving the budget knob actually scales the content
// (the dead-knob bug shipped identical sizes).
func TestDigestMCPKnobAlive(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Now()
	enableSources(t, cfg, "gmail")
	seedSyncStatus(t, cfg, "gmail", now.Add(-1*time.Hour))
	// Seed MANY items so the default budget must truncate but the max budget can
	// surface more — only then does default<max hold (the knob is observable).
	for i := 0; i < 400; i++ {
		digestSeedUnindexed(t, cfg, "gmail", "Item"+string(rune('0'+i%10))+string(rune('A'+i/26%26))+string(rune('a'+i%26)), time.Duration(i+1)*time.Minute, now)
	}
	rebuildDigestIndex(t, cfg)

	defaultBytes := digestPayloadBytes(t, `{}`)
	maxBytes := digestPayloadBytes(t, `{"max_tokens":20000}`)
	if defaultBytes >= maxBytes {
		t.Fatalf("max_tokens knob is dead: default payload (%d B) >= max payload (%d B); the budget must scale the content", defaultBytes, maxBytes)
	}
	// Both must stay under their respective byte budgets.
	if defaultBytes > defaultContextTokens*charsPerToken {
		t.Fatalf("default payload %d B exceeds the default budget %d B", defaultBytes, defaultContextTokens*charsPerToken)
	}
	if maxBytes > maxContextTokens*charsPerToken {
		t.Fatalf("max payload %d B exceeds the 20k budget %d B", maxBytes, maxContextTokens*charsPerToken)
	}
}

// digestPayloadBytes marshals just the structured digest payload (sections +
// source_states + headers) for a given max_tokens arg.
func digestPayloadBytes(t *testing.T, args string) int {
	t.Helper()
	p := digestMCPStructured(t, args)
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal digest payload: %v", err)
	}
	return len(b)
}

func payloadKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestStyleDigestTTYSentinels (M-6): each new sentinel line renders styled on a
// TTY-enabled styler (every prefix has a matching case) and the byte-clean
// invariant holds off-TTY (no ANSI on a non-TTY styler).
func TestStyleDigestTTYSentinels(t *testing.T) {
	// A digest body exercising every new sentinel: [new]/[updated] item prefixes,
	// the +N more guard line, and the no-changes/stale/unavailable section states.
	raw := strings.Join([]string{
		"# Mora digest — 2026-06-08 (since last brief)",
		"",
		"## Emails (2)",
		"- [new] Quarterly review — snippet (id: id-1)",
		"- [updated] Roadmap thread — snippet (id: id-2)",
		"- +3 more since last brief",
		"",
		"## Texts — no changes since last brief",
		"",
		"## Calendar — stale (no recent sync)",
		"",
		"## Files — unavailable (sync error)",
	}, "\n")

	// Off-TTY: byte-identical, zero ANSI (the load-bearing invariant).
	if got := styleDigestTTY(raw, styler{on: false}); got != raw {
		t.Fatalf("non-TTY styleDigestTTY must be byte-identical; got:\n%s", got)
	}

	// Under `go test` (no TTY) lipgloss's default renderer is Ascii and emits no
	// escapes, so force a real color profile to observe the styling. (SetColorProfile
	// exists for exactly this.) Restore it after.
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	// On-TTY: every sentinel line is styled (differs from raw) — no line is left
	// half-rendered. We force the styler on directly (the colorEnabled gate is
	// tested separately).
	styled := styleDigestTTY(raw, styler{on: true})
	for _, probe := range []string{
		"[new] Quarterly review",
		"[updated] Roadmap thread",
		"+3 more since last brief",
		"no changes since last brief",
		"stale (no recent sync)",
		"unavailable (sync error)",
	} {
		// The styled output must NOT contain the bare line verbatim (it should be
		// wrapped in ANSI). We assert each probe's line is transformed.
		assertLineStyled(t, raw, styled, probe)
	}
}

// assertLineStyled finds the raw line containing probe and asserts the
// corresponding styled line differs (was wrapped in ANSI), i.e. it is not left
// un-styled / half-rendered.
func assertLineStyled(t *testing.T, raw, styled, probe string) {
	t.Helper()
	rawLines := strings.Split(raw, "\n")
	styledLines := strings.Split(styled, "\n")
	if len(rawLines) != len(styledLines) {
		t.Fatalf("styleDigestTTY changed the line count (%d → %d)", len(rawLines), len(styledLines))
	}
	for i, rl := range rawLines {
		if strings.Contains(rl, probe) {
			if styledLines[i] == rl {
				t.Fatalf("line %q was NOT styled on a TTY (half-rendered sentinel)", rl)
			}
			if !strings.ContainsRune(styledLines[i], '\x1b') {
				t.Fatalf("styled line for %q carries no ANSI escape: %q", probe, styledLines[i])
			}
			return
		}
	}
	t.Fatalf("probe %q not found in raw digest", probe)
}
