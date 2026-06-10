package mora

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

// TestPulseDigestRendersSections: `mora pulse --digest` renders the real
// cross-source brief (not just task counts) with recent connector memories. As of
// Phase 12 the digest groups by sourceInstanceKey (== Provider) and is delta-aware,
// so the test seeds a provider-bearing memory + enables the source. The first
// preview is a cold-start, which displays the last-7-days courtesy window.
func TestPulseDigestRendersSections(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "gmail")
	now := time.Now()
	seedSyncStatus(t, cfg, "gmail", now.Add(-1*time.Hour))
	digestSeed(t, cfg, "gmail", "Quarterly review", 1*time.Hour, now)

	var out bytes.Buffer
	if err := Run(context.Background(), []string{"pulse", "--digest"}, &out, &out, nil); err != nil {
		t.Fatalf("pulse --digest: %v\n%s", err, out.String())
	}
	s := out.String()
	if !strings.Contains(s, "Mora digest") {
		t.Fatalf("pulse --digest should render the digest header; got:\n%s", s)
	}
	if !strings.Contains(s, "Quarterly review") {
		t.Fatalf("pulse --digest should include the recent memory; got:\n%s", s)
	}
}

// TestDigestMCPTool: the `digest` MCP tool returns ONE budgeted STRUCTURED
// payload inside a valid CallToolResult, so an agent (Codex) can read the typed
// delta directly. Plan 05 (D-05) removed the doubled render STRING — the payload
// is now the typed-delta `sections` (+ source_states), not a Markdown blob. The
// recent memory rides inside a section item. since_hours selects the plain-window
// path (SC#2), which still groups by instance key.
func TestDigestMCPTool(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "imessage")
	now := time.Now()
	digestSeed(t, cfg, "imessage", "Dinner Friday?", 1*time.Hour, now)

	text, isErr := mcpToolText(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"digest","arguments":{"since_hours":48}}}`)
	if isErr {
		t.Fatalf("digest tool errored: %s", text)
	}
	// The structured payload carries the item title inside sections — but NOT a
	// `"digest": "# Mora digest …"` render string (that doubling is gone).
	if !strings.Contains(text, "Dinner Friday?") {
		t.Fatalf("digest tool should surface the recent memory in a section item; got:\n%s", text)
	}
	if strings.Contains(text, "# Mora digest") {
		t.Fatalf("digest tool must NOT ship the Markdown render string (D-05 doubling removed); got:\n%s", text)
	}
}
