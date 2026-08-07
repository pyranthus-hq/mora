package mora

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func pinPrepClock(t *testing.T, at time.Time) {
	t.Helper()
	old := prepClock
	prepClock = func() time.Time { return at }
	t.Cleanup(func() { prepClock = old })
}

// TestMCPMeetingPrepRoundTrip: meeting_prep returns the same fully-cited
// unfinished-business shape as `mora brief --event-id`.
func TestMCPMeetingPrepRoundTrip(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	pinPrepClock(t, now)
	if err := saveSources(cfg, []Source{{
		Name: "gmail", Type: "gmail", Email: "me@a.com",
		Enabled: ptr(true), CreatedAt: now.Format(time.RFC3339),
	}}); err != nil {
		t.Fatal(err)
	}
	if err := writeMemory(cfg, meetingBriefEmail(
		"ask", "Deck follow-up", "Can you send the deck for the Acme sync by tomorrow?",
		"riya@a.com", []string{"me@a.com"}, now.Add(-time.Hour),
	)); err != nil {
		t.Fatal(err)
	}
	if err := writeMemory(cfg, eventMemFull("evt", "Acme sync", now.Add(2*time.Hour).Format(time.RFC3339),
		map[string]string{"riya@a.com": "Riya", "me@a.com": "Me"}, "me@a.com", "riya@a.com")); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	text, isErr := mcpToolText(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"meeting_prep","arguments":{"event_id":"evt","at":"2026-06-14T12:00:00Z"}}}`)
	if isErr {
		t.Fatalf("meeting_prep errored: %s", text)
	}
	for _, want := range []string{`"as_of"`, `"event"`, `"attendees"`, `"sections"`, `"egress_calls": 0`, `"citation"`, `"memory_id"`, `"open_loops"`, "Acme sync"} {
		if !strings.Contains(text, want) {
			t.Fatalf("meeting_prep payload missing %q:\n%s", want, text)
		}
	}
}

func TestMeetingPrepNameFallbackIsExplicit(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	pinPrepClock(t, now)
	if err := saveSources(cfg, []Source{{
		Name: "gmail", Type: "gmail", Email: "me@a.com",
		Enabled: ptr(true), CreatedAt: now.Format(time.RFC3339),
	}}); err != nil {
		t.Fatal(err)
	}
	if err := writeMemory(cfg, eventMemFull("evt", "General sync", now.Add(2*time.Hour).Format(time.RFC3339),
		map[string]string{"riya@a.com": "Riya", "me@a.com": "Me"}, "me@a.com", "riya@a.com")); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}

	fallback, err := buildNextMeetingBrief(context.Background(), cfg, now, map[string]bool{"person:missing@example.com": true}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !fallback.NameFallback || fallback.Event == nil || fallback.Event.ID != "evt" {
		t.Fatalf("fallback brief = %#v, want evt with name_fallback", fallback)
	}

	matched, err := buildNextMeetingBrief(context.Background(), cfg, now, map[string]bool{"person:riya@a.com": true}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if matched.NameFallback {
		t.Fatal("matching name filter must not report fallback")
	}
}

// TestMeetingPrepPayloadUnderCeiling (T0 stress): a heavy meeting (25 attendees ×
// 20 memories each) must still land under the 12000-token ceiling — per-attendee
// dossier budgets plus the global cited-line cap hold the line.
func TestMeetingPrepPayloadUnderCeiling(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	pinPrepClock(t, now)
	if err := saveSources(cfg, []Source{{
		Name: "gmail", Type: "gmail", Email: "me@x.com",
		Enabled: ptr(true), CreatedAt: now.Format(time.RFC3339),
	}}); err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	var attendees []string
	for i := 0; i < 25; i++ {
		addr := fmt.Sprintf("person%02d@x.com", i)
		attendees = append(attendees, addr)
		names[addr] = fmt.Sprintf("Person %02d", i)
		for j := 0; j < 20; j++ {
			if err := writeMemory(cfg, personMemNamed(fmt.Sprintf("e-%02d-%02d", i, j), "gmail", addr, names[addr], now.Add(-time.Duration(j+1)*time.Hour))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writeMemory(cfg, eventMemFull("evt", "Big sync", now.Add(2*time.Hour).Format(time.RFC3339), names, attendees...)); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	res, err := callMCPTool(context.Background(), "meeting_prep", map[string]any{"max_tokens": 20000})
	if err != nil {
		t.Fatal(err)
	}
	mp, ok := res.(MeetingBrief)
	if !ok {
		t.Fatalf("meeting_prep returned %T, want MeetingBrief", res)
	}
	if n := meetingBriefLineCount(mp); n > meetingPrepEvidenceCap {
		t.Fatalf("lines = %d, want capped at %d", n, meetingPrepEvidenceCap)
	}
	b, _ := json.Marshal(res)
	if tok := len(b) / charsPerToken; tok > 12000 {
		t.Fatalf("meeting_prep payload = %d tok > 12000 ceiling (%d bytes)", tok, len(b))
	}
}

// TestMCPDigestEntityParam: the digest tool's `entity` arg filters to one person.
func TestMCPDigestEntityParam(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	recent := time.Now().Add(-2 * time.Hour)
	if err := writeMemory(cfg, personMem("riya-mcp", "gmail", "riya@a.com", recent)); err != nil {
		t.Fatal(err)
	}
	if err := writeMemory(cfg, personMem("bob-mcp", "gmail", "bob@z.com", recent)); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	text, isErr := mcpToolText(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"digest","arguments":{"since_hours":24,"entity":"riya@a.com"}}}`)
	if isErr {
		t.Fatalf("digest entity errored: %s", text)
	}
	if !strings.Contains(text, "riya-mcp") || strings.Contains(text, "bob-mcp") {
		t.Fatalf("MCP digest entity filter wrong:\n%s", text)
	}
}

// TestMCPDigestEntityNoMatchErrors: an unknown entity returns an MCP error.
func TestMCPDigestEntityNoMatchErrors(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	if err := writeMemory(cfg, personMem("riya-mcp", "gmail", "riya@a.com", time.Now().Add(-2*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	text, isErr := mcpToolText(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"digest","arguments":{"entity":"ghost@nowhere.com"}}}`)
	if !isErr || !strings.Contains(strings.ToLower(text), "no entity") {
		t.Fatalf("no-match entity: isErr=%v text=%s, want an error", isErr, text)
	}
}

// TestMCPDigestNegativeSinceDaysNoOp (P1-D): since_days=-7 is clamped (no-op), not a
// future cutoff that empties the digest.
func TestMCPDigestNegativeSinceDaysNoOp(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	if err := writeMemory(cfg, personMem("riya-mcp", "gmail", "riya@a.com", time.Now().Add(-2*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	text, isErr := mcpToolText(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"digest","arguments":{"since_hours":24,"since_days":-7}}}`)
	if isErr {
		t.Fatalf("errored: %s", text)
	}
	if !strings.Contains(text, "riya-mcp") {
		t.Fatalf("negative since_days should be a no-op, got empty:\n%s", text)
	}
}

// TestMCPBriefEntityParam: the brief tool's `entity` arg filters (filter-aware path).
func TestMCPBriefEntityParam(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	pinBriefClock(t)
	now := briefFixedNow
	if err := writeMemory(cfg, personMem("riya-b", "gmail", "riya@a.com", now.Add(-2*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if err := writeMemory(cfg, personMem("bob-b", "gmail", "bob@z.com", now.Add(-2*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	text, isErr := mcpToolText(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"brief","arguments":{"entity":"riya@a.com"}}}`)
	if isErr {
		t.Fatalf("brief entity errored: %s", text)
	}
	if !strings.Contains(text, "riya-b") || strings.Contains(text, "bob-b") {
		t.Fatalf("MCP brief entity filter wrong:\n%s", text)
	}
}
