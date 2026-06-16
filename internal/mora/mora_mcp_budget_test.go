package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// MCP output-size regression gate ("T0").
//
// Every MCP tool's FULL CallToolResult envelope must serialize under a fixed
// per-tool token ceiling, anchored to maxContextTokens=20000 — Neil's redline
// that "one tool result must not dominate the window". We measure the whole
// result map (text content block + structuredContent mirror) because that is
// what the agent actually pays for on the wire, and because the doubling bug
// lives in the envelope: toCallToolResult (mora.go) JSON-marshals object-shaped
// returns into a text block AND mirrors the same value into structuredContent.
// Tokens are bytes/charsPerToken to match the codebase's own budget unit.
//
// Several tools are RED today BY DESIGN — the over-budget rows are the
// deliverable: they pin the real bugs (envelope doubling, the digest sidecar
// bypass, unbounded entity enumeration, uncapped evidence-body dumps). RED rows
// are quarantined via wantRED so CI stays green-on-known-issues, but the gate
// flips RED on two events: (a) a green tool regresses past its ceiling, or
// (b) a quarantined tool is FIXED (lands under its ceiling) — which forces the
// dev to flip wantRED:false and lock the win in as a new green gate. We never
// use t.Skip for RED rows (a skip is invisible and won't notice the fix), and
// we never scale a ceiling with the limit/max_tokens arg (a regression ceiling
// is a fixed line; scaling it would hide the regression it exists to catch).

// budgetCase is one row of the regression gate.
//
// Ceilings are TOKENS, all anchored to the maxContextTokens=20000 redline ("no
// single tool result may dominate the window") and tiered by role:
//   - mutation / point-read (write 1500=8%, read 4000=20%, delete 500=2.5%,
//     get_entity-404 12000): tiny — a point op must not approach the window.
//   - synthesis / briefing, meant to be DENSE (think 6000=30%, digest 10000=50%,
//     context 12000=60%): under or at half the window — a briefing that fills
//     the window defeats its own purpose.
//   - raw enumeration (search 8000=40%, list_memory 10000=50%, list_entities
//     8000=40%): row headroom, but a fixed cap that should force per-result
//     snippeting + limits before higher limits are unlocked.
//
// They are policy lines, not derived constants — the point is that they are
// FIXED, so a regression has something to cross.
type budgetCase struct {
	tool        string // subtest name / message label
	line        string // raw JSON-RPC tools/call line
	ceil        int    // hard ceiling in TOKENS (ceil(bytes / charsPerToken))
	wantRED     bool   // known-failing today; quarantined (CI green) but FAILS if it ever passes
	redBaseline int    // for wantRED: the known-bad token magnitude. FAILS if it WORSENS >25% (so 408KB→4MB can't hide). 0 = no worsening guard.
	mutates     bool   // write/delete: ordered last on the fixture; skipped in live mode
	note        string // tracking note for a known-RED row (names the bug + source line)
}

// budgetCall builds a tools/call JSON-RPC line. args is a raw JSON object string.
func budgetCall(name, args string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, name, args)
}

// measureEnvelope drives one tools/call and returns the full CallToolResult
// envelope size in bytes (the marshaled result map: text block + any
// structuredContent mirror).
func measureEnvelope(t *testing.T, line string) int {
	t.Helper()
	res := mcpResult(t, line)
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	return len(b)
}

// seedBudgetFixture builds a deterministic vault that reproduces the structural
// token blowups. It collapses 200 emails sharing one sender into a single
// high-degree person ("Neil Patel"), which is what get_entity dumps in full,
// while the 200 distinct recipients become ~200 low-degree persons that bloat
// list_entities. All timestamps/bodies are fixed (no time.Now / no randomness)
// so buildGraph + the static embedder produce byte-identical output every run.
func seedBudgetFixture(t *testing.T) Config {
	t.Helper()
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	body := strings.Repeat("lorem ipsum dolor sit amet ", 40) // ~1KB, stable
	for i := 0; i < 200; i++ {
		m := Memory{
			ID:        fmt.Sprintf("gmail_thread/t%04d", i),
			Scope:     "personal",
			Type:      "email",
			Title:     fmt.Sprintf("Re: thread %04d", i),
			CreatedAt: "2026-05-01T00:00:00Z", // FIXED — never time.Now()
			Source:    "gmail",
			Text:      body,
			Meta: map[string]any{
				"from":  []string{"neil@example.com"},
				"to":    []string{fmt.Sprintf("adit%04d@x.com", i)}, // distinct → no person collapse
				"names": map[string]string{"neil@example.com": "Neil Patel"},
			},
		}
		if err := writeMemory(cfg, m); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	// Recent, multi-source items so `digest` (a delta/cold-start window) has enough
	// IN-WINDOW volume to exercise the budget across many sections. Bodies are tiny
	// (digest snippets cap at digestSnippetLen=200 anyway). These carry no `meta`,
	// so they add no graph entities, and contain no "lorem", so search/context/
	// think ignore them.
	//
	// Dates are now-relative — the ONLY time-dependent part of the fixture —
	// pinned well inside the cold-start window so the digest size stays stable
	// run-to-run (timestamps only jitter the total by a few bytes).
	//
	// PHASE 12: the digest groups by sourceInstanceKey (== Provider) and is
	// DELTA-aware. A `{}` call is DELTA mode, which surfaces only ENABLED+INGESTING
	// catalog connectors and, on an instance's FIRST run (no watermark, true of
	// this fixture), displays a 7-day cold-start courtesy window. So the recent
	// items carry a REAL ingesting Provider (gmail/calendar/imessage), a
	// content_hash, and the sources are enabled — without the Provider these items
	// would be skipped and the digest would render empty (a FALSE-green that hid the
	// doubling). Plan 05 (D-05) shipped ONE budgeted structured payload (no render-
	// string doubling) with an ALIVE max_tokens knob, so the digest_default/
	// digest_max rows are now GREEN under the 20000 redline and digest_default <
	// digest_max (TestMCPDigestKnobIsAlive). The per-source cap here exceeds
	// digestDefaultCap so the MCP path (which surfaces mcpDigestMaxItems) has enough
	// in-window volume for the byte budget — not the cap — to govern, which is what
	// makes the knob observable.
	recent := time.Now().Add(-12 * time.Hour)
	enableSources(t, cfg, "gmail", "calendar", "imessage")
	seedSyncStatus(t, cfg, "gmail", recent)
	seedSyncStatus(t, cfg, "imessage", recent)
	seedSyncStatus(t, cfg, "calendar", recent)
	// gmail/imessage display by created_at (last 7d); calendar by upcoming 7d, so
	// its items are FUTURE-dated. perSource is generous so the byte budget (not the
	// per-source cap) governs how many items the MCP digest surfaces — the condition
	// under which max_tokens visibly scales the payload (digest_default<digest_max).
	const perSource = 40
	recentProviders := []struct {
		provider string
		future   bool
	}{{"gmail", false}, {"imessage", false}, {"calendar", true}}
	for _, rp := range recentProviders {
		for j := 0; j < perSource; j++ {
			created := recent.Add(-time.Duration(j) * time.Minute)
			if rp.future {
				created = time.Now().Add(time.Duration(j+1) * time.Hour) // upcoming 7d window
			}
			m := Memory{
				ID:          fmt.Sprintf("%s_item/%02d", rp.provider, j),
				Scope:       "personal",
				Type:        "event",
				Title:       fmt.Sprintf("%s item %02d standing weekly sync", rp.provider, j),
				CreatedAt:   created.UTC().Format(time.RFC3339),
				Source:      fmt.Sprintf("%s_item/%02d", rp.provider, j),
				Provider:    rp.provider,
				ProviderID:  fmt.Sprintf("%s_item/%02d", rp.provider, j),
				ContentHash: fmt.Sprintf("h-%s-%02d", rp.provider, j),
				Text:        fmt.Sprintf("recent in-window digest item %s-%d", rp.provider, j),
			}
			if err := writeMemory(cfg, m); err != nil {
				t.Fatalf("seed recent %s-%d: %v", rp.provider, j, err)
			}
		}
	}
	// Dedicated read/delete subjects with SLASH-FREE ids. Connector-style ids
	// ("gmail_thread/x") nest under memoryPath into a subdirectory whose file
	// base is just "x.md", which findMemory cannot resolve — so read_memory /
	// delete_memory on those return an isError envelope and silently measure
	// ~22 tok of nothing. (Production is fine: the real connector files via
	// writeMappedMemory+SafeFilename, the form findMemory matches; this only
	// bites the generic writeMemory path used here.) The read target carries a
	// realistically LARGE single body (~6KB long thread) so read_memory actually
	// exercises a real payload; both are non-"lorem" + old-dated so they don't
	// perturb search/context/digest/list_memory.
	readBody := strings.Repeat("a single long thread reply line goes here. ", 150) // ~6KB
	if err := writeMemory(cfg, Memory{
		ID: "read-target", Scope: "global", Type: "note",
		Title: "Read target", CreatedAt: "2026-05-03T00:00:00Z", Text: readBody,
	}); err != nil {
		t.Fatalf("seed read-target: %v", err)
	}
	if err := writeMemory(cfg, Memory{
		ID: "delete-target", Scope: "global", Type: "note",
		Title: "Delete target", CreatedAt: "2026-05-03T00:00:00Z", Text: "disposable",
	}); err != nil {
		t.Fatalf("seed delete-target: %v", err)
	}

	// Large-body matching memories so the snippet cap is exercised over long
	// bodies: a unique term "bulktext" + ~8KB bodies. Seed 9 (> the default
	// limit=8) so the no-arg search_default_limit case returns a FULL 8 rows —
	// proving 8 snippeted rows stay under budget, not just 6. Unique term + old
	// date keep them out of every other tool's results.
	bulkBody := strings.Repeat("bulktext padding sentence number goes here. ", 190) // ~8KB
	for i := 0; i < 9; i++ {
		if err := writeMemory(cfg, Memory{
			ID: fmt.Sprintf("bulk-%d", i), Scope: "global", Type: "note",
			Title: fmt.Sprintf("Bulk %d bulktext", i), CreatedAt: "2026-05-03T00:00:00Z", Text: bulkBody,
		}); err != nil {
			t.Fatalf("seed bulk %d: %v", i, err)
		}
	}

	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatalf("rebuildIndex: %v", err)
	}
	return cfg
}

// TestMCPSearchDefaultLimitIsEight proves the no-arg search_memory default is
// mcpSearchDefaultLimit (bumped 5→8) and that every returned body is snippet-
// capped. The fixture seeds 9 "bulktext" rows, so a default search returns 8.
func TestMCPSearchDefaultLimitIsEight(t *testing.T) {
	seedBudgetFixture(t)
	res, err := callMCPTool(context.Background(), "search_memory", map[string]any{"query": "bulktext"})
	if err != nil {
		t.Fatalf("search_memory: %v", err)
	}
	obj, ok := res.(map[string]any)
	if !ok {
		t.Fatalf("search_memory returned %T, want map with results+freshness", res)
	}
	mems, ok := obj["results"].([]Memory)
	if !ok {
		t.Fatalf("search_memory results = %T, want []Memory", obj["results"])
	}
	if len(mems) != mcpSearchDefaultLimit {
		t.Fatalf("default search_memory returned %d results, want %d (the bumped default limit)", len(mems), mcpSearchDefaultLimit)
	}
	for _, m := range mems {
		// snippet keeps searchSnippetLen content runes + a trailing ellipsis.
		if n := len([]rune(m.Text)); n > searchSnippetLen+1 {
			t.Fatalf("result %s body = %d runes > snippet cap %d+ellipsis (snippetMemories not applied)", m.ID, n, searchSnippetLen)
		}
		if !m.Truncated {
			t.Fatalf("result %s has a clipped body but Truncated=false", m.ID)
		}
	}
}

// budgetCases is the contract table. Ceilings are in tokens, anchored to the
// 20000-token redline and tiered by role: mutation/read-one tools are tiny
// point ops; synthesis/briefing tools (think/digest/context) sit well under
// half the window because density is their value; raw enumerations get row
// headroom but a fixed cap that should force per-result snippeting + limits.
func budgetCases() []budgetCase {
	return []budgetCase{
		// read-only point lookups
		{tool: "read_memory", line: budgetCall("read_memory", `{"id":"read-target"}`), ceil: 4000},
		// search default slice (small bodies) is the regression guard; search_big
		// USED to pin the body-bloat bug (full Memory.Text per row, no cap) — now
		// FIXED: the MCP path snippets each body (snippetMemories, searchSnippetLen).
		{tool: "search_memory", line: budgetCall("search_memory", `{"query":"lorem","limit":5}`), ceil: 8000},
		{tool: "search_big", line: budgetCall("search_memory", `{"query":"bulktext","limit":5}`), ceil: 8000,
			note: "FIXED: snippetMemories caps each row at searchSnippetLen=240, so even long bodies stay well under budget."},
		// The bumped default (limit=8, no arg) must also stay budget-safe over long
		// bodies — proves snippeting, not the old limit=5, is what holds the line.
		{tool: "search_default_limit", line: budgetCall("search_memory", `{"query":"bulktext"}`), ceil: 8000},
		// list_memory's bug (full bodies, no snippet cap) is body-SIZE driven, not
		// count driven: on the live vault it is 27.7KB@limit=10 / 313KB@limit=50,
		// but over this ~1KB-body fixture it stays ~3k tok. Forcing it would mean
		// oversized bodies that couple search/read/get_entity, making the fixture
		// fragile. Kept green here (regression guard for the default slice);
		// the real blowup is caught by live mode + the per-row snippet fix.
		{tool: "list_memory", line: budgetCall("list_memory", `{"limit":10}`), ceil: 10000},

		// graph reads — the headline blowups
		{tool: "list_entities", line: budgetCall("list_entities", `{}`), ceil: 8000,
			note: "GREEN (v0.5.1): compact projection — memory_ids dropped, salience-ranked (entities.go entitiesForMCP)"},
		{tool: "get_entity_notfound", line: budgetCall("get_entity", `{"name":"Nobody Here"}`), ceil: 12000},
		{tool: "get_entity_found", line: budgetCall("get_entity", `{"name":"Neil Patel"}`), ceil: 12000,
			note: "GREEN (v0.5.1): bodies snippeted + memories/edges/neighbors capped (entities.go entityMemoriesForMCP)"},

		// briefing / synthesis
		{tool: "context_default", line: budgetCall("context_memory", `{"query":"lorem"}`), ceil: 12000},
		// context is object-doubled by the envelope AND its content is capped at
		// 10 hybridSearch hits, so on the live vault max_tokens=20000 marshals to
		// ~2× the budget (a single call can claim the whole 20k window). This
		// fixture's 10 uniform ~1KB hits can't fill the bigger budget, so it stays
		// green here; the doubling/whole-window risk is live-verified. The fix
		// (drop the structuredContent mirror, or halve the effective budget) is
		// shared with digest. Kept as a default-slice regression guard.
		{tool: "context_max", line: budgetCall("context_memory", `{"query":"lorem","max_tokens":20000}`), ceil: 12000},
		{tool: "think", line: budgetCall("think", `{"query":"lorem"}`), ceil: 6000},
		// Plan 05 (D-05) FIXED both digest bugs: the MCP `digest` tool now ships ONE
		// budgeted STRUCTURED payload (typed-delta sections + source_states) — no
		// duplicate render string beside the sections (doubling removed) — and that
		// payload actually scales with max_tokens (the knob is alive again). So the
		// ceiling is raised to the owner's full 20000 redline and both rows flip GREEN:
		// each lands well UNDER 20000, digest_default lands under its default budget,
		// and digest_default < digest_max (asserted in TestMCPDigestKnobIsAlive) proves
		// the knob lives. The remaining envelope text+structuredContent mirror is the
		// generic toCallToolResult shape shared by every object tool, NOT D-05's
		// digest-internal doubling — tracked separately, out of this plan's scope.
		{tool: "digest_default", line: budgetCall("digest", `{}`), ceil: 20000,
			note: "D-05 fixed: one budgeted structured payload (no render-string doubling), lands under the default budget"},
		{tool: "digest_max", line: budgetCall("digest", `{"max_tokens":20000}`), ceil: 20000,
			note: "D-05 fixed: max_tokens knob alive — digest_max > digest_default (asserted in TestMCPDigestKnobIsAlive), both under the 20000 redline"},
		// Phase 15 (D15-4): the opt-in envelope variant — cited items PLUS a
		// synthesis_prompt — must STILL land under the 20000 redline at max_tokens.
		// The prompt-reserved budget (budgetEnvelopePayload reserves
		// envelopePromptReserve, then budgets items against budgetChars−reserve, and
		// the synthesis_prompt's per-item lines reuse the ALREADY-budgeted Snippet
		// bodies — additive plain text on the base map) is what holds the ceiling. NO
		// wantRED: this is GREEN by design; if it ever measures over, the reservation
		// is wrong and must be fixed — do NOT loosen the ceiling to hide it (the gate
		// is the forcing function, per this file's own discipline). This row is the
		// standing guard that the synthesis_prompt can never silently blow the window.
		{tool: "digest_envelope", line: budgetCall("digest", `{"envelope":true,"max_tokens":20000}`), ceil: 20000,
			note: "Phase 15: envelope-on (cited items + synthesis_prompt) must STILL land under the 20000 redline — the prompt-reserved budget (budgetEnvelopePayload) holds the ceiling (D15-4)"},
		// Phase 16 (D16-3): the session-start `brief` tool reuses the digest budget
		// machinery verbatim — briefDigest builds the SAME delta/window Digest the
		// digest case feeds, then digestMCPPayload (plain) / budgetEnvelopePayload
		// (envelope) budget it against the SAME ceiling. So both variants MUST land
		// under the 20000 redline at max_tokens, exactly like digest_max /
		// digest_envelope. NO wantRED: GREEN by design — the brief tool inherits the
		// proven reservation. If either ever measures over, the brief surface (not the
		// ceiling) is wrong and must be fixed; do NOT loosen the line (the gate is the
		// forcing function, per this file's discipline). These are the standing guard
		// that the session-start brief can never silently blow the MCP window.
		{tool: "brief", line: budgetCall("brief", `{"max_tokens":20000}`), ceil: 20000,
			note: "Phase 16: the session-start brief tool reuses the digest budget machinery (digestMCPPayload) — must land under the 20000 redline at max_tokens (D16-3)"},
		{tool: "brief_envelope", line: budgetCall("brief", `{"envelope":true,"max_tokens":20000}`), ceil: 20000,
			note: "Phase 16: brief envelope-on (cited items + synthesis_prompt via budgetEnvelopePayload) must STILL land under the 20000 redline — the same prompt-reserved budget as digest_envelope holds the ceiling (D16-3)"},

		// mutations — keep LAST so they don't perturb the fixture for the reads above.
		// write_memory echoes the caller's own text (already in their context) and
		// object-doubles it; tiny here, but the 2× is the real waste. delete echoes
		// only {deleted:id}.
		{tool: "write_memory", line: budgetCall("write_memory", `{"title":"x","text":"`+strings.Repeat("y", 2000)+`"}`), ceil: 1500, mutates: true},
		{tool: "delete_memory", line: budgetCall("delete_memory", `{"id":"delete-target"}`), ceil: 500, mutates: true},
	}
}

// assertBudget applies the gate semantics to one measured row.
func assertBudget(t *testing.T, c budgetCase, tok, bytes int) {
	t.Helper()
	switch {
	case c.wantRED && tok <= c.ceil:
		t.Fatalf("FIXED: %s now %d tok ≤ ceiling %d — flip wantRED:false to lock this in as a green gate (was tracking: %s)",
			c.tool, tok, c.ceil, c.note)
	case c.wantRED && c.redBaseline > 0 && tok > c.redBaseline*5/4:
		// A quarantined tool got WORSE (not fixed): 408KB→4MB must not stay green.
		t.Fatalf("WORSENED: %s = %d tok > 1.25× baseline %d (still over ceiling %d) — a known-RED tool regressed further; investigate, then re-baseline. Tracking: %s",
			c.tool, tok, c.redBaseline, c.ceil, c.note)
	case c.wantRED:
		t.Logf("known-RED %s: %d tok > %d (%d B) — tracked: %s", c.tool, tok, c.ceil, bytes, c.note)
	case tok > c.ceil:
		t.Fatalf("REGRESSION: %s = %d B = %d tok > ceiling %d (redline 20000)", c.tool, bytes, tok, c.ceil)
	default:
		t.Logf("ok %s: %d tok ≤ %d (%d B)", c.tool, tok, c.ceil, bytes)
	}
}

// TestMCPBudgetCeilings is the deterministic CI gate. It exits 0 on the known
// baseline (RED rows logged, not failed) and FAILS only on a real regression or
// on a quarantined tool getting fixed.
func TestMCPBudgetCeilings(t *testing.T) {
	seedBudgetFixture(t) // MCP server reaches it via loadConfig() under the temp HOME

	for _, c := range budgetCases() {
		c := c
		t.Run(c.tool, func(t *testing.T) {
			b := measureEnvelope(t, c.line)
			tok := (b + charsPerToken - 1) / charsPerToken // ceil: ceilings are hard lines
			assertBudget(t, c, tok, b)
		})
	}
}

// TestMCPDigestKnobIsAlive locks D-05's central proof: the max_tokens knob
// actually scales the digest payload. Measured on the SAME budget fixture, the
// envelope for max_tokens=20000 must be STRICTLY LARGER than the default, and the
// default must land under the default token budget. Before the fix the two were
// byte-identical (the `sections` sidecar ignored max_tokens) — this is the
// regression guard that the knob stays alive.
func TestMCPDigestKnobIsAlive(t *testing.T) {
	seedBudgetFixture(t)

	defB := measureEnvelope(t, budgetCall("digest", `{}`))
	maxB := measureEnvelope(t, budgetCall("digest", `{"max_tokens":20000}`))
	defTok := (defB + charsPerToken - 1) / charsPerToken
	maxTok := (maxB + charsPerToken - 1) / charsPerToken

	if defTok >= maxTok {
		t.Fatalf("max_tokens knob is DEAD: digest_default %d tok >= digest_max %d tok — the payload must scale with max_tokens", defTok, maxTok)
	}
	if defTok > defaultContextTokens {
		t.Fatalf("digest_default %d tok must land under the default budget %d tok", defTok, defaultContextTokens)
	}
	if maxTok > maxContextTokens {
		t.Fatalf("digest_max %d tok must land under the 20000 redline", maxTok)
	}
}

// TestMCPDigestNoDoublingInEnvelope guards D-05's other half: the digest payload
// must not ship a render STRING duplicate of the typed sections. The
// structuredContent mirror carries the typed payload; a `"digest"` key beside
// `"sections"` would be the same content twice.
func TestMCPDigestNoDoublingInEnvelope(t *testing.T) {
	seedBudgetFixture(t)
	res := mcpResult(t, budgetCall("digest", `{}`))
	sc, ok := res["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("digest result missing object structuredContent: %v", res)
	}
	if _, hasRender := sc["digest"]; hasRender {
		t.Fatalf("digest payload ships a render string AND typed sections (doubling) — D-05 requires one budgeted payload")
	}
	if _, hasSections := sc["sections"]; !hasSections {
		t.Fatalf("digest payload must ship the typed `sections`")
	}
}

// TestMCPGateDigestEnvelopeOffByteIdentical pins SC#4 in the GATE FILE — the
// canonical "what the MCP surface ships" contract — not just the 15-02 wiring
// file. The envelope-OFF digest payload must be byte-identical to the plain
// digestMCPPayload: `digest {}` and `digest {"envelope":false}` produce the same
// marshaled structuredContent, and NEITHER carries a `synthesis_prompt` key. A
// future change that leaked the envelope into the default (off) path — breaking
// the byte-identical backward-compat guarantee — fails CI here (T-15-09). The
// envelope-ON positive control then locks the off/on distinction so a later
// change cannot make them silently identical (the off path must NOT gain the
// prompt, AND the on path must keep it). Measured on the SAME seedBudgetFixture
// the rest of the gate runs over, so the contract is anchored to the budget gate.
func TestMCPGateDigestEnvelopeOffByteIdentical(t *testing.T) {
	// Freeze the clock to one instant so the two digest generations below stamp an
	// identical header timestamp; a second ticking between them would otherwise break
	// the byte-identical comparison (a pre-existing wall-clock flake). Frozen at
	// real-now so the now-12h budget fixture stays inside the digest window.
	frozen := time.Now()
	oldClock := briefClock
	briefClock = func() time.Time { return frozen }
	t.Cleanup(func() { briefClock = oldClock })

	seedBudgetFixture(t)

	empty := digestMCPStructured(t, `{}`)
	off := digestMCPStructured(t, `{"envelope":false}`)

	// (1) Neither off-path marshaled form may carry a synthesis_prompt key — the
	// off path is the unchanged D-05 payload.
	if _, has := empty["synthesis_prompt"]; has {
		t.Fatalf("plain digest {} must NOT carry synthesis_prompt; keys=%v", payloadKeys(empty))
	}
	if _, has := off["synthesis_prompt"]; has {
		t.Fatalf("envelope:false must NOT carry synthesis_prompt; keys=%v", payloadKeys(off))
	}

	// (2) {} and {"envelope":false} marshal byte-for-byte the same — the durable
	// SC#4 regression guard pinned in the gate file.
	emptyB, err := json.Marshal(empty)
	if err != nil {
		t.Fatalf("marshal {} payload: %v", err)
	}
	offB, err := json.Marshal(off)
	if err != nil {
		t.Fatalf("marshal envelope:false payload: %v", err)
	}
	if !bytes.Equal(emptyB, offB) {
		t.Fatalf("digest {} and {envelope:false} must be byte-identical (SC#4)\n {}: %s\noff: %s", emptyB, offB)
	}

	// (3) Positive control: envelope:true DOES carry a non-empty synthesis_prompt,
	// so the off/on distinction is locked — a future change cannot make them
	// silently identical (the gate would otherwise pass on two empty payloads).
	on := digestMCPStructured(t, `{"envelope":true}`)
	prompt, ok := on["synthesis_prompt"].(string)
	if !ok || strings.TrimSpace(prompt) == "" {
		t.Fatalf("envelope:true must carry a non-empty synthesis_prompt (off/on must differ); keys=%v", payloadKeys(on))
	}
}

// TestMCPBudgetLive measures the SAME contracts against a real vault for a
// one-shot report. Opt-in via MORA_BUDGET_LIVE=/path/to/vault; skipped in CI.
// It runs only non-mutating tools and never rebuilds the live index.
//
// NOTE: the fastest way to get live numbers is actually the stdio binary, e.g.
//
//	printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_entities","arguments":{}}}' \
//	  | mora mcp serve | wc -c
//
// This Go path exists for parity but requires read-only config repointing,
// which is left as a focused follow-up rather than risking the live index.
func TestMCPBudgetLive(t *testing.T) {
	vault := os.Getenv("MORA_BUDGET_LIVE")
	if vault == "" {
		t.Skip("set MORA_BUDGET_LIVE=/path/to/vault to measure a real vault (or use `mora mcp serve | wc -c`)")
	}
	t.Skip("live mode: wire read-only config repoint at " + vault + " before enabling (see doc comment)")
}
