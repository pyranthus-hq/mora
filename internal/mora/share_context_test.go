package mora

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Pilot F5/F9 (devbox fresh-install pilot, 2026-10-09): on a machine that only
// subscribes, the session-start surfaces (context, brief, context_memory) read
// only the local vault, so an agent there concludes it has no memory. These
// contracts pin the fix: the query-shaped surfaces union subscribed shares,
// owner-attributed, and the query-less surfaces say shares exist and name the
// tools that read them. Every surface stays byte-identical with zero shares.

const sharedAcmeID = "mem_20260601_000000_aaaaaaaa"

func subscribeAcme(t *testing.T, cfg Config) {
	t.Helper()
	setupSubscription(t, cfg, "neil", []Memory{
		fixtureMemory(sharedAcmeID, "Acme widgets", "acme ships widgets in q3"),
	})
}

func mcpJSON(t *testing.T, tool string, args map[string]any) (string, map[string]any) {
	t.Helper()
	got, err := callMCPTool(testCtx(t), tool, args)
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return string(b), m
}

func TestContextQueryCLIUnionsSharedCorpusWithAttribution(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	subscribeAcme(t, cfg)

	out := run(t, "context", "--query", "acme widgets")
	if !strings.Contains(out, "# Acme widgets") {
		t.Fatalf("context --query missing the shared memory:\n%s", out)
	}
	if !strings.Contains(out, `Shared: from subscribed share "neil"`) {
		t.Fatalf("context --query missing share attribution:\n%s", out)
	}
	jout := run(t, "context", "--query", "acme widgets", "--json")
	if !strings.Contains(jout, `"owner": "neil"`) || !strings.Contains(jout, sharedAcmeID) {
		t.Fatalf("context --json items missing the owner-attributed shared row:\n%s", jout)
	}
}

func TestMCPContextMemoryUnionsSharedCorpusWithAttribution(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	subscribeAcme(t, cfg)

	raw, out := mcpJSON(t, "context_memory", map[string]any{"query": "acme widgets"})
	text, _ := out["context"].(string)
	if !strings.Contains(text, "# Acme widgets") || !strings.Contains(text, `Shared: from subscribed share "neil"`) {
		t.Fatalf("context_memory text missing the attributed shared memory:\n%s", text)
	}
	if !strings.Contains(raw, `"evidence_id":"`+sharedAcmeID+`"`) || !strings.Contains(raw, `"canonical_source_id":"share:neil"`) {
		t.Fatalf("context_memory evidence_manifest does not cite the share:\n%s", raw)
	}
	if _, ok := out["shared_corpora"]; ok {
		t.Fatalf("a query that already read the shares must not also point at them:\n%s", raw)
	}
}

// The query path with zero subscriptions (and with a subscription that has no
// hit) returns exactly the local retrieval: same rows, same order, same scores.
func TestContextQueryZeroShareIsLocalRetrievalUnchanged(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	seedAuthored(t, "personal", "Local sqlite note", "we picked sqlite locally")
	seedAuthored(t, "personal", "Second sqlite note", "sqlite again")
	now := time.Now()

	want, err := hybridSearch(testCtx(t), cfg, "sqlite", "", 10, searchFilters{})
	if err != nil {
		t.Fatal(err)
	}
	if len(want) == 0 {
		t.Fatal("fixture: expected local hits")
	}
	got, _, _, err := contextQueryData(testCtx(t), cfg, "sqlite", "", 10, searchFilters{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("zero-share context changed local retrieval:\n got %+v\nwant %+v", got, want)
	}

	subscribeAcme(t, cfg) // shares exist, but none matches "sqlite"
	got, _, _, err = contextQueryData(testCtx(t), cfg, "sqlite", "", 10, searchFilters{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("a non-matching share changed local retrieval:\n got %+v\nwant %+v", got, want)
	}
	if text := buildContext(cfg, got, 1<<20, true); strings.Contains(text, "Shared: from subscribed share") {
		t.Fatalf("local rows rendered with a share attribution:\n%s", text)
	}
}

// Without a query there is nothing to rank shares against, so context_memory
// says they exist and names the tools that read them, instead of returning
// only vault control files as if the machine knew nothing.
func TestMCPContextMemoryNoQueryPointsAtShares(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	raw, out := mcpJSON(t, "context_memory", map[string]any{})
	if _, ok := out["shared_corpora"]; ok {
		t.Fatalf("zero-share context_memory grew a shared_corpora key:\n%s", raw)
	}

	subscribeAcme(t, cfg)
	raw, out = mcpJSON(t, "context_memory", map[string]any{})
	assertSharedCorporaPointer(t, raw, out)
}

func TestThinkCLIShowsShareOwner(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	subscribeAcme(t, cfg)

	out := run(t, "think", "acme widgets")
	if !strings.Contains(out, "["+sharedAcmeID+"] [neil] Acme widgets") {
		t.Fatalf("think CLI hides the share attribution:\n%s", out)
	}
}

func TestMCPThinkReturnsAttributedSharedEvidence(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	subscribeAcme(t, cfg)

	raw, _ := mcpJSON(t, "think", map[string]any{"query": "acme widgets"})
	if !strings.Contains(raw, `"stable_id":"`+sharedAcmeID+`"`) || !strings.Contains(raw, `"owner":"neil"`) {
		t.Fatalf("MCP think missing the attributed shared evidence:\n%s", raw)
	}
}

func assertSharedCorporaPointer(t *testing.T, raw string, out map[string]any) {
	t.Helper()
	sc, ok := out["shared_corpora"].(map[string]any)
	if !ok {
		t.Fatalf("missing shared_corpora pointer:\n%s", raw)
	}
	if sc["subscriptions"] != float64(1) || sc["memories"] != float64(1) {
		t.Fatalf("shared_corpora counts wrong: %+v", sc)
	}
	note, _ := sc["note"].(string)
	for _, want := range []string{"search_memory", "think", "context_memory"} {
		if !strings.Contains(note, want) {
			t.Fatalf("shared_corpora note does not name %s: %q", want, note)
		}
	}
	shares, _ := sc["shares"].([]any)
	if len(shares) != 1 || !strings.Contains(raw, `"name":"neil"`) {
		t.Fatalf("shared_corpora does not list the subscription: %+v", sc)
	}
}

// Brief is a delta over the user's own connectors. It does not summarize
// shared corpora; it says they exist and points at the tools that read them.
func TestMCPBriefPointsAtSharesWhenSubscribed(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	subscribeAcme(t, cfg)

	raw, out := mcpJSON(t, "brief", map[string]any{})
	assertSharedCorporaPointer(t, raw, out)

	raw, out = mcpJSON(t, "brief", map[string]any{"envelope": true})
	assertSharedCorporaPointer(t, raw, out)
}

// With zero shares the MCP brief is byte-identical to the pre-change payload,
// which is digestMCPPayload over briefDigest at the same clock.
func TestMCPBriefZeroShareByteIdentical(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	seedAuthored(t, "personal", "Local sqlite note", "we picked sqlite locally")
	now := time.Date(2026, 10, 9, 21, 0, 0, 0, time.UTC)
	old := briefClock
	briefClock = func() time.Time { return now }
	t.Cleanup(func() { briefClock = old })

	got, err := callMCPTool(testCtx(t), "brief", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	d, err := briefDigest(cfg, now, mcpDigestMaxItems)
	if err != nil {
		t.Fatal(err)
	}
	want := digestMCPPayload(cfg, d, mcpDigestBudgetChars(cfg, 0))
	gb, _ := json.Marshal(got)
	wb, _ := json.Marshal(want)
	if string(gb) != string(wb) {
		t.Fatalf("zero-share brief changed:\n got %s\nwant %s", gb, wb)
	}
}

// The pointer is paid for out of the item budget, so a subscribed brief never
// returns more bytes than the caller asked for.
func TestMCPBriefSharePointerStaysWithinBudget(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	for i := 0; i < 40; i++ {
		seedAuthored(t, "personal", "Local sqlite note "+strings.Repeat("x", i), strings.Repeat("we picked sqlite locally ", 20))
	}
	subscribeAcme(t, cfg)
	const maxTokens = 1000
	raw, out := mcpJSON(t, "brief", map[string]any{"max_tokens": maxTokens})
	assertSharedCorporaPointer(t, raw, out)
	sections, _ := json.Marshal(out["sections"])
	noteBytes, _ := json.Marshal(out["shared_corpora"])
	if budget := mcpDigestBudgetChars(cfg, maxTokens); len(sections)+len(noteBytes) > budget {
		t.Fatalf("sections (%d) + pointer (%d) exceed the %d-byte budget", len(sections), len(noteBytes), budget)
	}
}

func TestCLIBriefPointsAtSharesWhenSubscribed(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	if out := run(t, "brief", "--fresh"); strings.Contains(out, "subscribed share") {
		t.Fatalf("zero-share brief mentions shares:\n%s", out)
	}
	if out := run(t, "brief", "--fresh", "--json"); strings.Contains(out, "shared_corpora") {
		t.Fatalf("zero-share brief --json grew shared_corpora:\n%s", out)
	}

	subscribeAcme(t, cfg)
	out := run(t, "brief", "--fresh")
	if !strings.Contains(out, "1 subscribed share (1 memory) is not summarized in this brief") || !strings.Contains(out, "mora search") {
		t.Fatalf("brief does not point at subscribed shares:\n%s", out)
	}
	jout := run(t, "brief", "--fresh", "--json")
	if !strings.Contains(jout, `"shared_corpora"`) || !strings.Contains(jout, `"name": "neil"`) {
		t.Fatalf("brief --json missing shared_corpora:\n%s", jout)
	}
}
