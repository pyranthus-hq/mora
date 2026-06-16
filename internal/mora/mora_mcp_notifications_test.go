package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestMCPNotificationsGetNoResponse locks the JSON-RPC contract that the server
// MUST NOT reply to notifications (messages with no "id"). Antigravity's strict
// MCP client (the official modelcontextprotocol go-sdk) sends
// notifications/initialized immediately after initialize and aborts the whole
// session — dropping every mora tool with `tools/list: invalid request` — if the
// server answers that notification with a stray `-32601 method not found` frame.
// Lenient clients (Claude Code, Codex) tolerate the stray frame, which is why
// this hid until the Antigravity integration check.
func TestMCPNotificationsGetNoResponse(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`,
		// A second, unrelated notification proves the rule is general (every
		// id-less message is ignored), not special-cased to initialized.
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":2}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
	}, "\n") + "\n"

	var out bytes.Buffer
	if err := Run(context.Background(), []string{"mcp", "serve"}, &out, &out, strings.NewReader(in)); err != nil {
		t.Fatalf("mcp serve: %v\n%s", err, out.String())
	}

	var ids []any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var frame struct {
			ID    any             `json:"id"`
			Error json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			t.Fatalf("non-JSON frame on the wire: %v\n%s", err, line)
		}
		// A response with no id is a reply to a notification — forbidden by
		// JSON-RPC 2.0 and fatal to strict MCP clients.
		if frame.ID == nil {
			t.Fatalf("server emitted a response with no id (a reply to a notification): %s", line)
		}
		if len(frame.Error) != 0 {
			t.Fatalf("server emitted a JSON-RPC error frame during a clean handshake: %s", line)
		}
		ids = append(ids, frame.ID)
	}
	// Exactly two replies: initialize (id 1) and tools/list (id 2). The
	// notification between them must produce nothing on the wire.
	if len(ids) != 2 {
		t.Fatalf("expected exactly 2 response frames (initialize + tools/list), got %d:\n%s", len(ids), out.String())
	}
}
