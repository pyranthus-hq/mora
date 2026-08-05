package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// brief_cmd_test.go drives Phase 16-02: the WIRING of resolveBrief (16-01) onto
// the two session-start surfaces — the `mora brief` CLI command and the MCP
// `brief` tool. RED-first.
//
// Both surfaces are LOCAL + read-only by construction (resolveBrief / briefDigest
// call only buildDigest{advance:false} — no sync, no watermark mutation), and the
// CLI is byte-clean (ANSI never reaches --json). The MCP tool ships the SAME
// budgeted structured payload the `digest` tool ships (D16-3), reusing the
// Phase-15 budget machinery, so it is budget-bounded and T0-safe by construction.

// briefFixedNow is a fixed UTC instant so the CLI/MCP wiring tests inherit the
// resolver's clock-independence (mirrors resolveFixedNow in brief_resolve_test.go).
var briefFixedNow = time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)

// pinBriefClock freezes the session-start brief surfaces' wall clock to
// briefFixedNow for the duration of the test, so a brief file dated briefFixedNow
// reads as "fresh" no matter the real calendar date. Without this the CLI/MCP
// wiring tests inherit real time.Now() and regenerate instead of reading the
// seeded file verbatim once today drifts past 2026-06-08 (the verbatim tests'
// documented clock-independence was never actually wired before this seam).
func pinBriefClock(t *testing.T) {
	t.Helper()
	old := briefClock
	briefClock = func() time.Time { return briefFixedNow }
	t.Cleanup(func() { briefClock = old })
}

// runBrief invokes the `mora brief` command via the public Run dispatcher with a
// captured stdout, asserting no error. It exercises the REAL Run switch wiring
// (case "brief"), not cmdBrief directly, so the dispatch case is covered too.
func runBrief(t *testing.T, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	full := append([]string{"brief"}, args...)
	if err := Run(context.Background(), full, &out, &out, strings.NewReader("")); err != nil {
		t.Fatalf("Run(brief %v) error: %v\noutput:\n%s", args, err, out.String())
	}
	return out.String()
}

// seedBriefCLIVault seeds an enabled, recently-synced gmail source with a real
// cold-start delta so `mora brief` GENERATES a non-empty brief (no persisted file
// present). Returns the cfg + the now to use (today's UTC day == briefFixedNow).
func seedBriefCLIVault(t *testing.T) (Config, time.Time) {
	t.Helper()
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := briefFixedNow
	enableSources(t, cfg, "gmail")
	seedSyncStatus(t, cfg, "gmail", now.Add(-1*time.Hour))
	digestSeed(t, cfg, "gmail", "CLI cold start thread", 2*time.Hour, now)
	return cfg, now
}

// --- Task 1: mora brief CLI -------------------------------------------------

// TestCmdBriefPrintsFreshFileVerbatim: when a FRESH persisted brief exists, the
// CLI prints it BYTE-FOR-BYTE (no re-style, no re-render). The persisted artifact
// is raw Markdown (writeBriefArtifact = renderDigest, no ANSI); the verbatim path
// must not double-skin it. We seed today's UTC-dated file directly so the resolver
// reads it verbatim (generated==false).
func TestCmdBriefPrintsFreshFileVerbatim(t *testing.T) {
	pinBriefClock(t)
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	// A distinctive Markdown sentinel that must survive the print untouched.
	const sentinel = "# Mora brief — VERBATIM SENTINEL\n\n## Emails (1)\n- [new] Distinctive thread (id: gmail_thread/x)\n"
	seedBriefFile(t, cfg, briefFixedNow.UTC().Format("2006-01-02"), sentinel)

	out := runBrief(t)
	if !strings.Contains(out, sentinel) {
		t.Fatalf("mora brief did not print the fresh persisted file verbatim\n--- got ---\n%q\n--- want substring ---\n%q", out, sentinel)
	}
	// No ANSI escape may have been injected (double-skinning / re-styling guard):
	// the persisted file is raw Markdown and must reach stdout unchanged.
	if strings.ContainsRune(out, '\x1b') {
		t.Fatalf("mora brief re-styled the verbatim persisted file (ANSI present); must print it raw:\n%q", out)
	}
}

// TestCmdBriefGeneratesWhenNoFreshFile: with no persisted brief, the CLI
// GENERATES a non-empty brief from the local vault and prints it.
func TestCmdBriefGeneratesWhenNoFreshFile(t *testing.T) {
	pinBriefClock(t)
	seedBriefCLIVault(t)
	out := runBrief(t)
	if strings.TrimSpace(out) == "" {
		t.Fatalf("mora brief printed nothing on the generate path, want a non-empty brief")
	}
	if !strings.Contains(out, "CLI cold start thread") {
		t.Fatalf("generated brief should surface the seeded thread; got:\n%s", out)
	}
}

// TestCmdBriefJSON: `--json` emits a typed {generated, body} object with NO ANSI
// escape bytes (byte-clean invariant: ANSI never reaches --json).
func TestCmdBriefJSON(t *testing.T) {
	pinBriefClock(t)
	seedBriefCLIVault(t)
	out := runBrief(t, "--json")
	if strings.ContainsRune(out, '\x1b') {
		t.Fatalf("mora brief --json emitted ANSI escape bytes (byte-clean invariant broken):\n%q", out)
	}
	var got briefResult
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("mora brief --json is not valid {generated, body} JSON: %v\n%s", err, out)
	}
	if !got.Generated {
		t.Fatalf("mora brief --json generated=false on the generate path, want true")
	}
	if !strings.Contains(got.Body, "CLI cold start thread") {
		t.Fatalf("mora brief --json body should carry the generated brief; got:\n%s", got.Body)
	}
}

// TestCmdBriefJSONFreshFileGeneratedFalse: `--json` over a FRESH persisted file
// reports generated=false and carries the file's verbatim body.
func TestCmdBriefJSONFreshFileGeneratedFalse(t *testing.T) {
	pinBriefClock(t)
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	const sentinel = "# Mora brief — JSON VERBATIM\n\nthis body is read from disk, not generated.\n"
	seedBriefFile(t, cfg, briefFixedNow.UTC().Format("2006-01-02"), sentinel)

	out := runBrief(t, "--json")
	var got briefResult
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("mora brief --json invalid: %v\n%s", err, out)
	}
	if got.Generated {
		t.Fatalf("mora brief --json generated=true on a fresh persisted file, want false (verbatim read)")
	}
	if got.Body != sentinel {
		t.Fatalf("mora brief --json body must equal the file verbatim\n got: %q\nwant: %q", got.Body, sentinel)
	}
}

// TestCmdBriefEnvelopeAppendsSynthesisPrompt: `--envelope` (non-json) appends the
// model-free synthesis_prompt (the grounding instruction the agent runs with its
// OWN model) after the brief body. Default (no --envelope) does NOT.
func TestCmdBriefEnvelopeAppendsSynthesisPrompt(t *testing.T) {
	seedBriefCLIVault(t)

	plain := runBrief(t)
	if strings.Contains(plain, "Cite each claim by its [id]") {
		t.Fatalf("plain `mora brief` must NOT append the synthesis_prompt; got:\n%s", plain)
	}

	env := runBrief(t, "--envelope")
	for _, want := range []string{"Cite each claim by its [id]", "read_memory"} {
		if !strings.Contains(env, want) {
			t.Fatalf("mora brief --envelope must append the grounding instruction %q; got:\n%s", want, env)
		}
	}
}

// TestCmdBriefNoNetwork is the zero-egress guard (D16-2/T-16-05): `mora brief`
// over a vault with enabled sources but NO live fetcher wired makes no network
// call — it completes purely from local disk. The run-helper never wires a live
// fetcher, so a network attempt would either error or block; a clean local
// completion proves the local-only contract behaviorally.
func TestCmdBriefNoNetwork(t *testing.T) {
	pinBriefClock(t)
	seedBriefCLIVault(t)
	// A bare `mora brief` must return promptly with local content — no sync flag
	// exists on the command, and resolveBrief/briefDigest never call a backfill.
	out := runBrief(t)
	if !strings.Contains(out, "CLI cold start thread") {
		t.Fatalf("local-only brief should surface the local thread without any sync; got:\n%s", out)
	}
}

// TestCmdBriefDoesNotAdvanceWatermark: a `mora brief` run NEVER mutates the
// Phase-12 watermark (read-only invariant, T-16-06). We baseline the watermark,
// run the command, and assert the snapshot is byte-identical afterward.
func TestCmdBriefDoesNotAdvanceWatermark(t *testing.T) {
	cfg, now := seedBriefCLIVault(t)
	// Baseline the watermark on disk so there's a snapshot to compare.
	if _, _, err := advanceBrief(cfg, now, briefOpts{advance: true}, 1<<20, false); err != nil {
		t.Fatalf("baseline advance: %v", err)
	}
	snapPath := briefPath(cfg, "gmail")
	before, err := os.ReadFile(snapPath)
	if err != nil {
		t.Fatalf("read snapshot before: %v", err)
	}

	_ = runBrief(t)

	after, err := os.ReadFile(snapPath)
	if err != nil {
		t.Fatalf("read snapshot after: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("mora brief mutated the watermark snapshot (read-only invariant broken)\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
}

// --- Task 1: briefDigest helper ---------------------------------------------

// TestBriefDigestDeltaThenWindowFallback pins the factored generate semantics: a
// DELTA preview first; when the delta surfaces ZERO items (the scheduled --advance
// job already consumed it), re-build in the fixed 24h WINDOW so the brief is never
// useless. Neither build advances the watermark.
func TestBriefDigestDeltaThenWindowFallback(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := briefFixedNow
	enableSources(t, cfg, "gmail")
	seedSyncStatus(t, cfg, "gmail", now.Add(-1*time.Hour))
	digestSeed(t, cfg, "gmail", "Window only thread", 2*time.Hour, now)

	// Consume the delta so a plain DELTA preview surfaces nothing.
	if _, _, err := advanceBrief(cfg, now, briefOpts{advance: true}, 1<<20, false); err != nil {
		t.Fatalf("seed-advance: %v", err)
	}

	d, err := briefDigest(cfg, now, mcpDigestMaxItems)
	if err != nil {
		t.Fatalf("briefDigest: %v", err)
	}
	if surfacedItemCount(d) == 0 {
		t.Fatalf("briefDigest returned an empty digest; the 24h-window fallback should surface the recent item")
	}
	if collectTitles(d) == "" || !strings.Contains(collectTitles(d), "Window only thread") {
		t.Fatalf("briefDigest window fallback should surface the recent thread; titles=%q", collectTitles(d))
	}
}

// TestMCPBriefWindowFallbackUsesLatestConversationActivity reproduces a
// long-lived iMessage miss: connector rewrites preserve the
// thread's original created_at while meta.occurred_at advances with its latest
// message. Once the scheduled brief has consumed the content-hash delta, the MCP
// brief's 24h fallback must still surface the freshly active conversation.
func TestMCPBriefWindowFallbackUsesLatestConversationActivity(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := briefFixedNow

	oldClock := briefClock
	briefClock = func() time.Time { return now }
	t.Cleanup(func() { briefClock = oldClock })

	enableSources(t, cfg, "imessage")
	seedSyncStatus(t, cfg, "imessage", now.Add(-time.Hour))
	m := Memory{
		ID:          "imessage_chat/taylor",
		Scope:       "global",
		Type:        "imessage",
		Title:       "Taylor",
		Text:        "Taylor: Still good for tomorrow?\nMe: How's 11am for me?\nTaylor: Sure.",
		Source:      "imessage_chat/taylor",
		Provider:    "imessage",
		ProviderID:  "imessage_chat/taylor",
		ContentHash: "taylor-current-hash",
		CreatedAt:   now.Add(-30 * 24 * time.Hour).Format(time.RFC3339),
		Meta: map[string]any{
			"participants": []map[string]string{{"handle": "+15550102020", "name": "Taylor"}},
			"occurred_at":  now.Add(-time.Hour).Format(time.RFC3339),
		},
	}
	if err := writeMemory(cfg, m); err != nil {
		t.Fatalf("writeMemory: %v", err)
	}
	rebuildDigestIndex(t, cfg)

	// The scheduled advance already saw this exact revision, so DELTA is empty
	// and MCP must take the 24h WINDOW fallback where the regression lived.
	if err := saveBriefSnapshot(cfg, briefSnapshot{
		Key:   "imessage",
		Items: map[string]string{m.ID: m.ContentHash},
	}, now.Add(-30*time.Minute)); err != nil {
		t.Fatalf("saveBriefSnapshot: %v", err)
	}

	// Keep this regression on the MCP brief's digest/window mechanics. The daily
	// commitment inventory has its own eligibility contract and is orthogonal to
	// the connector timestamp mismatch exercised here.
	raw, err := mcpBrief(context.Background(), ungatedDigestConfig(cfg), map[string]any{})
	if err != nil {
		t.Fatalf("mcpBrief: %v", err)
	}
	p, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("mcpBrief returned %T, want structured payload", raw)
	}
	if got := p["since_hours"]; got != briefFallbackWindowHours {
		t.Fatalf("MCP brief did not take the 24h fallback: since_hours=%v", got)
	}
	b, err := json.Marshal(p["sections"])
	if err != nil {
		t.Fatalf("marshal sections: %v", err)
	}
	if !bytes.Contains(b, []byte(`"Taylor"`)) {
		t.Fatalf("MCP brief dropped the freshly active long-lived conversation; sections=%s", b)
	}
}

// collectTitles concatenates every section item title (a tiny test helper).
func collectTitles(d Digest) string {
	var b strings.Builder
	for _, s := range d.Sections {
		for _, it := range s.Items {
			b.WriteString(it.Title)
			b.WriteString(" ")
		}
	}
	return b.String()
}

// --- Task 2: MCP brief tool -------------------------------------------------

// briefMCPStructured calls the MCP `brief` tool and returns the decoded structured
// result map (the structuredContent mirror == the raw tool return). Mirrors
// digestMCPStructured.
func briefMCPStructured(t *testing.T, args string) map[string]any {
	t.Helper()
	line := budgetCall("brief", args)
	res := mcpResult(t, line)
	sc, ok := res["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("brief result missing object structuredContent: %v", res)
	}
	return sc
}

// TestMCPBriefInToolsList: the `brief` tool is advertised in tools/list with the
// max_tokens + envelope params, so an MCP agent can discover it.
func TestMCPBriefInToolsList(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	line := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	res := mcpResult(t, line)
	tools, ok := res["tools"].([]any)
	if !ok || len(tools) == 0 {
		t.Fatalf("tools/list returned no tools: %v", res)
	}
	var brief map[string]any
	for _, raw := range tools {
		m, _ := raw.(map[string]any)
		if m["name"] == "brief" {
			brief = m
			break
		}
	}
	if brief == nil {
		t.Fatalf("tools/list does not advertise a `brief` tool")
	}
	schema, _ := brief["inputSchema"].(map[string]any)
	props, _ := schema["properties"].(map[string]any)
	for _, p := range []string{"max_tokens", "envelope"} {
		if _, ok := props[p]; !ok {
			t.Fatalf("brief tool schema missing %q param; props=%v", p, props)
		}
	}
}

// TestMCPBriefPayload (D16-3): the default brief tool result is digest-SHAPED —
// it carries the digest payload keys (sections, source_states, generated,
// since_hours) and NO synthesis_prompt — the same ONE budgeted payload as digest.
func TestMCPBriefPayload(t *testing.T) {
	seedDigestVault(t)
	p := briefMCPStructured(t, `{}`)
	for _, key := range []string{"sections", "source_states", "generated", "since_hours"} {
		if _, ok := p[key]; !ok {
			t.Fatalf("MCP brief payload must carry %q (digest-shaped); got keys: %v", key, payloadKeys(p))
		}
	}
	if _, ok := p["synthesis_prompt"]; ok {
		t.Fatalf("plain MCP brief must NOT carry synthesis_prompt; keys=%v", payloadKeys(p))
	}
	// No render-string doubling (the same contract the digest payload holds).
	if _, ok := p["digest"]; ok {
		t.Fatalf("MCP brief payload must NOT ship a `digest` render string beside `sections`; keys=%v", payloadKeys(p))
	}
}

// TestMCPBriefEnvelope (SC#3): envelope=true returns a non-empty synthesis_prompt
// plus the same keys, and every id in the returned sections appears in the prompt
// (no-dangling end-to-end) — exactly the digest-tool contract.
func TestMCPBriefEnvelope(t *testing.T) {
	seedDigestVault(t)
	on := briefMCPStructured(t, `{"envelope":true}`)
	prompt, ok := on["synthesis_prompt"].(string)
	if !ok || strings.TrimSpace(prompt) == "" {
		t.Fatalf("envelope:true must carry a non-empty synthesis_prompt; keys=%v", payloadKeys(on))
	}
	for _, key := range []string{"sections", "source_states", "generated", "since_hours"} {
		if _, ok := on[key]; !ok {
			t.Fatalf("envelope brief must still carry %q; keys=%v", key, payloadKeys(on))
		}
	}
	secs, _ := on["sections"].([]any)
	cited := 0
	for _, s := range secs {
		sm, _ := s.(map[string]any)
		items, _ := sm["items"].([]any)
		for _, it := range items {
			im, _ := it.(map[string]any)
			id, _ := im["id"].(string)
			if id == "" {
				continue
			}
			if !strings.Contains(prompt, "["+id+"]") {
				t.Fatalf("brief synthesis_prompt must cite returned id [%s]; prompt:\n%s", id, prompt)
			}
			cited++
		}
	}
	if cited == 0 {
		t.Fatalf("expected the envelope brief to cite at least one returned item; prompt:\n%s", prompt)
	}
}

// TestMCPBriefEnvelopeOffByteIdentical: brief {} and {"envelope":false} return
// byte-identical structuredContent with no synthesis_prompt — the off path is
// unperturbed (mirrors the digest tool's contract).
func TestMCPBriefEnvelopeOffByteIdentical(t *testing.T) {
	seedDigestVault(t)
	empty := briefMCPStructured(t, `{}`)
	off := briefMCPStructured(t, `{"envelope":false}`)
	if _, has := empty["synthesis_prompt"]; has {
		t.Fatalf("plain brief {} must NOT carry synthesis_prompt; keys=%v", payloadKeys(empty))
	}
	// `generated` is a wall-clock stamp that legitimately differs between two
	// separate invocations (the two calls can straddle a second on a slow runner);
	// the envelope contract is about everything ELSE being identical.
	delete(empty, "generated")
	delete(off, "generated")
	emptyB, _ := json.Marshal(empty)
	offB, _ := json.Marshal(off)
	if !bytes.Equal(emptyB, offB) {
		t.Fatalf("brief {} and {envelope:false} must be byte-identical\n {}: %s\noff: %s", emptyB, offB)
	}
}

// TestMCPBriefHonorsMaxTokens (D16-3 / T-16-09): max_tokens=20000 yields a payload
// at least as large as the default (~6000) and under the 20k byte ceiling — the
// budget knob is alive and the tool is T0-bounded. We seed many items so the
// default budget must truncate while the max budget can surface strictly more.
func TestMCPBriefHonorsMaxTokens(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Now()
	enableSources(t, cfg, "gmail")
	seedSyncStatus(t, cfg, "gmail", now.Add(-1*time.Hour))
	for i := 0; i < 400; i++ {
		digestSeedUnindexed(t, cfg, "gmail", "Item"+string(rune('0'+i%10))+string(rune('A'+i/26%26))+string(rune('a'+i%26)), time.Duration(i+1)*time.Minute, now)
	}
	rebuildDigestIndex(t, cfg)

	defaultBytes := briefPayloadBytes(t, `{}`)
	maxBytes := briefPayloadBytes(t, `{"max_tokens":20000}`)
	if defaultBytes > maxBytes {
		t.Fatalf("brief max_tokens knob shrank the payload: default %d B > max %d B", defaultBytes, maxBytes)
	}
	if maxBytes > maxContextTokens*charsPerToken {
		t.Fatalf("brief max payload %d B exceeds the 20k byte ceiling %d B", maxBytes, maxContextTokens*charsPerToken)
	}
}

// briefPayloadBytes marshals the structured brief payload for a given args object.
func briefPayloadBytes(t *testing.T, args string) int {
	t.Helper()
	p := briefMCPStructured(t, args)
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal brief payload: %v", err)
	}
	return len(b)
}

// TestMCPBriefReadOnly (T-16-05/T-16-06): calling the MCP brief tool advances
// NOTHING — the watermark snapshot is byte-identical across the call (no sync, no
// --advance). Proves the tool is local + read-only by construction.
func TestMCPBriefReadOnly(t *testing.T) {
	cfg := seedDigestVault(t)
	now := time.Now()
	// Baseline the watermark so there is a snapshot file to compare.
	if _, _, err := advanceBrief(cfg, now, briefOpts{advance: true}, 1<<20, false); err != nil {
		t.Fatalf("baseline advance: %v", err)
	}
	snapPath := briefPath(cfg, "gmail")
	before, err := os.ReadFile(snapPath)
	if err != nil {
		t.Fatalf("read snapshot before: %v", err)
	}

	_ = briefMCPStructured(t, `{}`)

	after, err := os.ReadFile(snapPath)
	if err != nil {
		t.Fatalf("read snapshot after: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("MCP brief mutated the watermark snapshot (read-only invariant broken)\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
}

// TestMCPBriefInstructionsMentionsBrief: the server-level mcpInstructions name
// `brief` as the session-start default, so a fresh agent discovers the habit.
func TestMCPBriefInstructionsMentionsBrief(t *testing.T) {
	if !strings.Contains(mcpInstructions, "brief") {
		t.Fatalf("mcpInstructions must mention `brief` as the session-start default; got:\n%s", mcpInstructions)
	}
}
