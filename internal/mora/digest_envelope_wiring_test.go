package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// digest_envelope_wiring_test.go — Plan 15-02: the opt-in envelope wiring onto the
// MCP `digest` tool and `pulse --digest`, plus the budgetEnvelopePayload assembler
// and the boolArg helper. The two hard invariants pinned here:
//   1. envelope OFF ⇒ byte-for-byte the existing digestMCPPayload / pulse output
//      (SC#4 backward-compat; the T0 gate + the plain digest tests depend on it).
//   2. envelope ON ⇒ a non-empty synthesis_prompt built from the SAME budgeted
//      sections the payload emits, so every cited id is an item the agent receives
//      (no-dangling, SC#3); the prompt's fixed template is reserved so a tight
//      budget never truncates the instructions away.

// ---------------------------------------------------------------------------
// Task 1: boolArg + budgetEnvelopePayload (assembler-level invariants)
// ---------------------------------------------------------------------------

// TestBoolArg pins the lenient MCP bool parser: a real Go bool, a "true"/"false"
// string from a lenient client, and the default when absent/untypable.
func TestBoolArg(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		def  bool
		want bool
	}{
		{"native true", map[string]any{"envelope": true}, false, true},
		{"native false", map[string]any{"envelope": false}, true, false},
		{"string true", map[string]any{"envelope": "true"}, false, true},
		{"string false", map[string]any{"envelope": "false"}, true, false},
		{"absent uses def true", map[string]any{}, true, true},
		{"absent uses def false", map[string]any{}, false, false},
		{"untypable uses def", map[string]any{"envelope": 3.0}, false, false},
		{"untypable uses def true", map[string]any{"envelope": []any{}}, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := boolArg(tc.args, "envelope", tc.def); got != tc.want {
				t.Fatalf("boolArg(%v, envelope, %v) = %v, want %v", tc.args, tc.def, got, tc.want)
			}
		})
	}
}

// seededEnvelopeDigest builds a real Digest from a seeded vault so the assembler
// tests run against genuine budgeted sections (not a synthetic struct).
func seededEnvelopeDigest(t *testing.T, cfg Config, now time.Time) Digest {
	t.Helper()
	d, err := buildDigest(cfg, now, briefOpts{perSourceCap: mcpDigestMaxItems})
	if err != nil {
		t.Fatalf("buildDigest: %v", err)
	}
	return d
}

// collectIDs returns every DigestItem.ID across a section slice (the ids the
// agent actually receives — the only ids the prompt is allowed to cite).
func collectIDs(sections []DigestSection) []string {
	var ids []string
	for _, s := range sections {
		for _, it := range s.Items {
			ids = append(ids, it.ID)
		}
	}
	return ids
}

// TestBudgetEnvelopePayloadGroundedFromBudgetedSections: the assembler builds the
// synthesis_prompt from the SAME sections it returns, so the prompt cites exactly
// the budgeted ids and never a dropped one (no-dangling at the assembler level).
func TestBudgetEnvelopePayloadGroundedFromBudgetedSections(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Now()
	enableSources(t, cfg, "gmail")
	seedSyncStatus(t, cfg, "gmail", now.Add(-1*time.Hour))
	for i := 0; i < 6; i++ {
		digestSeed(t, cfg, "gmail", "Thread "+string(rune('A'+i)), time.Duration(i+1)*time.Hour, now)
	}
	d := seededEnvelopeDigest(t, cfg, now)

	env := budgetEnvelopePayload(cfg, d, defaultContextTokens*charsPerToken)
	if strings.TrimSpace(env.SynthesisPrompt) == "" {
		t.Fatalf("budgetEnvelopePayload must return a non-empty synthesis_prompt")
	}
	ids := collectIDs(env.Sections)
	if len(ids) == 0 {
		t.Fatalf("expected at least one budgeted item to cite; got none")
	}
	// Every id present in the returned sections must be cited by the prompt.
	for _, id := range ids {
		if !strings.Contains(env.SynthesisPrompt, "["+id+"]") {
			t.Fatalf("synthesis_prompt must cite budgeted id [%s]; prompt:\n%s", id, env.SynthesisPrompt)
		}
	}
}

// TestBudgetEnvelopePayloadPromptSurvivesTightBudget: under a tiny budget the
// items are squeezed but the synthesis instructions (the fixed grounding header)
// are NEVER truncated away — the reservation works.
func TestBudgetEnvelopePayloadPromptSurvivesTightBudget(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Now()
	enableSources(t, cfg, "gmail")
	seedSyncStatus(t, cfg, "gmail", now.Add(-1*time.Hour))
	for i := 0; i < 50; i++ {
		digestSeed(t, cfg, "gmail", "Thread "+string(rune('A'+i%26))+string(rune('0'+i%10)), time.Duration(i+1)*time.Minute, now)
	}
	d := seededEnvelopeDigest(t, cfg, now)

	// A deliberately tight budget — smaller than the prompt reserve itself in the
	// degenerate direction — must still carry the full grounding instructions.
	env := budgetEnvelopePayload(cfg, d, 200)
	if strings.TrimSpace(env.SynthesisPrompt) == "" {
		t.Fatalf("synthesis_prompt must survive a tight budget (the reserve protects the instructions)")
	}
	for _, want := range []string{"grounded ONLY in the cited items", "Cite each claim by its [id]", "read_memory"} {
		if !strings.Contains(env.SynthesisPrompt, want) {
			t.Fatalf("tight-budget prompt must keep the grounding instruction %q; prompt:\n%s", want, env.SynthesisPrompt)
		}
	}
	// And it still must not cite a dropped id — only what the budgeted sections hold.
	for _, id := range collectIDs(env.Sections) {
		if !strings.Contains(env.SynthesisPrompt, "["+id+"]") {
			t.Fatalf("tight-budget prompt must cite budgeted id [%s]", id)
		}
	}
}

// TestBudgetEnvelopePayloadBaseFieldsMatchPlainPayload: the envelope's base fields
// (generated/since_hours/sections/source_states/freshness/stale_tasks) equal the
// plain digestMCPPayload produced against the SAME prompt-reserved budget — the
// envelope is a strict superset, the base subset is unchanged.
func TestBudgetEnvelopePayloadBaseFieldsMatchPlainPayload(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Now()
	enableSources(t, cfg, "gmail")
	seedSyncStatus(t, cfg, "gmail", now.Add(-1*time.Hour))
	for i := 0; i < 5; i++ {
		digestSeed(t, cfg, "gmail", "Thread "+string(rune('A'+i)), time.Duration(i+1)*time.Hour, now)
	}
	d := seededEnvelopeDigest(t, cfg, now)
	budget := defaultContextTokens * charsPerToken

	env := budgetEnvelopePayload(cfg, d, budget)
	// The envelope budgets items against the proportional envelope share (15-03/
	// D15-4: envelopeItemsBudget reserves ~1/3 of the compact budget for the
	// additive per-item synthesis_prompt), so the base subset must equal the plain
	// payload produced against that SAME item budget — not the raw budget.
	plain := digestMCPPayload(cfg, d, envelopeItemsBudget(budget))

	// Marshal the env's base subset (drop synthesis_prompt) and compare to plain.
	envBase := map[string]any{
		"generated":     env.Generated,
		"since_hours":   env.SinceHours,
		"sections":      env.Sections,
		"source_states": env.SourceStates,
		"freshness":     env.Freshness,
		"stale_tasks":   env.StaleTasks,
	}
	gotB, _ := json.Marshal(envBase)
	wantB, _ := json.Marshal(plain)
	if !bytes.Equal(gotB, wantB) {
		t.Fatalf("envelope base subset must equal the plain budgeted payload\n got: %s\nwant: %s", gotB, wantB)
	}
}

// ---------------------------------------------------------------------------
// Task 2: opt-in envelope on the MCP digest tool
// ---------------------------------------------------------------------------

// TestMCPDigestEnvelopeOffByteIdentical (SC#4, T-15-04): the digest tool called
// with {} and with {"envelope":false} returns the EXACT same structuredContent —
// byte-identical, no synthesis_prompt key — proving the off path is unperturbed.
func TestMCPDigestEnvelopeOffByteIdentical(t *testing.T) {
	seedDigestVault(t)

	empty := digestMCPStructured(t, `{}`)
	off := digestMCPStructured(t, `{"envelope":false}`)

	if _, has := empty["synthesis_prompt"]; has {
		t.Fatalf("plain digest {} must NOT carry synthesis_prompt; keys=%v", payloadKeys(empty))
	}
	if _, has := off["synthesis_prompt"]; has {
		t.Fatalf("envelope:false must NOT carry synthesis_prompt; keys=%v", payloadKeys(off))
	}
	emptyB, _ := json.Marshal(empty)
	offB, _ := json.Marshal(off)
	if !bytes.Equal(emptyB, offB) {
		t.Fatalf("digest {} and {envelope:false} must be byte-identical\n {}: %s\noff: %s", emptyB, offB)
	}
	// And both must still carry the plain payload's sections (not an empty shell).
	if _, ok := off["sections"]; !ok {
		t.Fatalf("envelope:false must still carry the plain `sections`; keys=%v", payloadKeys(off))
	}
}

// TestMCPDigestEnvelopeOn (SC#1/SC#3, T-15-05): envelope:true returns a non-empty
// synthesis_prompt PLUS the same sections/source_states keys, and every id in the
// returned sections appears in the prompt (no-dangling end-to-end).
func TestMCPDigestEnvelopeOn(t *testing.T) {
	seedDigestVault(t)

	on := digestMCPStructured(t, `{"envelope":true}`)
	prompt, ok := on["synthesis_prompt"].(string)
	if !ok || strings.TrimSpace(prompt) == "" {
		t.Fatalf("envelope:true must carry a non-empty synthesis_prompt; keys=%v", payloadKeys(on))
	}
	for _, key := range []string{"sections", "source_states", "generated", "since_hours"} {
		if _, ok := on[key]; !ok {
			t.Fatalf("envelope:true must still carry %q; keys=%v", key, payloadKeys(on))
		}
	}
	// no-dangling end-to-end: every id in the returned sections is cited.
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
				t.Fatalf("synthesis_prompt must cite returned id [%s]; prompt:\n%s", id, prompt)
			}
			cited++
		}
	}
	if cited == 0 {
		t.Fatalf("expected the envelope to cite at least one returned item; prompt:\n%s", prompt)
	}
}

// ---------------------------------------------------------------------------
// Task 3: pulse --digest --envelope
// ---------------------------------------------------------------------------

// seedPulseVault seeds an enabled gmail source with recent items so pulse --digest
// renders a real brief. Returns cfg + the now used (so the test can rebuild the
// same Digest for a byte-equality check).
func seedPulseVault(t *testing.T) (Config, time.Time) {
	t.Helper()
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Now()
	enableSources(t, cfg, "gmail")
	seedSyncStatus(t, cfg, "gmail", now.Add(-1*time.Hour))
	for i := 0; i < 4; i++ {
		digestSeed(t, cfg, "gmail", "Thread "+string(rune('A'+i)), time.Duration(i+1)*time.Hour, now)
	}
	return cfg, now
}

// TestPulseDigestEnvelopeOffUnchanged: plain `pulse --digest` output is byte-for-
// byte unchanged by the new --envelope flag (it does not appear unless set).
func TestPulseDigestEnvelopeOffUnchanged(t *testing.T) {
	seedPulseVault(t)

	var out bytes.Buffer
	if err := Run(context.Background(), []string{"pulse", "--digest"}, &out, &out, nil); err != nil {
		t.Fatalf("pulse --digest: %v\n%s", err, out.String())
	}
	got := out.String()
	if strings.Contains(got, "grounded ONLY in the cited items") {
		t.Fatalf("plain pulse --digest must NOT emit the synthesis_prompt; got:\n%s", got)
	}
	if strings.Contains(got, "CITED ITEMS:") {
		t.Fatalf("plain pulse --digest must NOT emit the envelope CITED ITEMS block; got:\n%s", got)
	}
	if !strings.Contains(got, "Mora digest") {
		t.Fatalf("plain pulse --digest must still render the digest brief; got:\n%s", got)
	}
}

// TestPulseDigestEnvelopeOnAppendsPrompt: `pulse --digest --envelope` output STARTS
// WITH the exact plain brief and then additionally carries the grounding prompt +
// the cited ids (the envelope is appended, the brief untouched).
func TestPulseDigestEnvelopeOnAppendsPrompt(t *testing.T) {
	seedPulseVault(t)

	var plainBuf bytes.Buffer
	if err := Run(context.Background(), []string{"pulse", "--digest"}, &plainBuf, &plainBuf, nil); err != nil {
		t.Fatalf("plain pulse --digest: %v\n%s", err, plainBuf.String())
	}
	var envBuf bytes.Buffer
	if err := Run(context.Background(), []string{"pulse", "--digest", "--envelope"}, &envBuf, &envBuf, nil); err != nil {
		t.Fatalf("pulse --digest --envelope: %v\n%s", err, envBuf.String())
	}
	plain := plainBuf.String()
	enriched := envBuf.String()

	if !strings.HasPrefix(enriched, plain) {
		t.Fatalf("--envelope output must START WITH the exact plain brief (it appends, never alters)\nplain:\n%s\nenriched:\n%s", plain, enriched)
	}
	if !strings.Contains(enriched, "grounded ONLY in the cited items") {
		t.Fatalf("--envelope must append the grounding synthesis_prompt; got:\n%s", enriched)
	}
	// The cited ids must be the rendered digest's item ids.
	d, err := buildDigest(mustConfig(t), time.Now(), briefOpts{})
	if err != nil {
		t.Fatalf("buildDigest: %v", err)
	}
	ids := collectIDs(d.Sections)
	if len(ids) == 0 {
		t.Fatalf("expected the rendered digest to have items to cite")
	}
	for _, id := range ids {
		if !strings.Contains(enriched, "["+id+"]") {
			t.Fatalf("--envelope must cite rendered id [%s]; got:\n%s", id, enriched)
		}
	}
}
