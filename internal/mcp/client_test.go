package mcp

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestSessionRemembersInitializeClientName(t *testing.T) {
	s := &Session{}
	if got := ClientName(s.Context(context.Background())); got != "" {
		t.Fatalf("before initialize = %q", got)
	}
	s.Observe(Request{Method: "tools/list", Params: json.RawMessage(`{"clientInfo":{"name":"ignored"}}`)})
	s.Observe(Request{Method: "initialize", Params: json.RawMessage(`not json`)})
	if got := ClientName(s.Context(context.Background())); got != "" {
		t.Fatalf("non-initialize or malformed params set a name: %q", got)
	}
	s.Observe(Request{Method: "initialize", Params: json.RawMessage(`{"protocolVersion":"2024-11-05","clientInfo":{"name":"claude-code","version":"1"}}`)})
	if got := ClientName(s.Context(context.Background())); got != "claude-code" {
		t.Fatalf("after initialize = %q", got)
	}
}

func TestMemoryFromArgsReadsAgent(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m, err := MemoryFromArgs(map[string]any{"title": "t", "text": "x", "agent": "Test-Bot"}, now, nil)
	if err != nil || m.Agent != "test-bot" {
		t.Fatalf("agent = %q, %v", m.Agent, err)
	}
	m, err = MemoryFromArgs(map[string]any{"title": "t", "text": "x"}, now, nil)
	if err != nil || m.Agent != "" {
		t.Fatalf("absent agent = %q, %v", m.Agent, err)
	}
	for _, bad := range []any{"two words", 7, "x\nid: y"} {
		if _, err := MemoryFromArgs(map[string]any{"title": "t", "text": "x", "agent": bad}, now, nil); err == nil {
			t.Fatalf("agent %#v accepted", bad)
		}
	}
}
