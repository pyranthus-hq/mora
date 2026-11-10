package mora

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	mcppkg "github.com/pyranthus-hq/mora/internal/mcp"
)

type popupFrame struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params struct {
		Message string `json:"message"`
		Schema  struct {
			Properties map[string]struct {
				Type    string `json:"type"`
				Title   string `json:"title"`
				Hint    string `json:"description"`
				Default bool   `json:"default"`
			} `json:"properties"`
			Required []string `json:"required"`
		} `json:"requestedSchema"`
	} `json:"params"`
	Result struct {
		IsError bool           `json:"isError"`
		Content map[string]any `json:"structuredContent"`
	} `json:"result"`
	Error json.RawMessage `json:"error"`
}

type popupWire struct {
	t      *testing.T
	encode *json.Encoder
	frames <-chan popupFrame
	errors <-chan error
}

// Use the real initialize/handler context over two pipes, never a synthetic
// elicitation context. All configs and writes live beneath withTempHome.
func startPopupWire(t *testing.T, ctx context.Context, capable bool) *popupWire {
	t.Helper()
	in, send := io.Pipe()
	receive, out := io.Pipe()
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- serveMCP(ctx, out, in) }()
	frames := make(chan popupFrame, 8)
	errors := make(chan error, 1)
	go func() {
		defer close(frames)
		decoder := json.NewDecoder(receive)
		for {
			var f popupFrame
			if err := decoder.Decode(&f); err != nil {
				errors <- err
				return
			}
			frames <- f
		}
	}()
	t.Cleanup(func() {
		cancel()
		for _, closer := range []io.Closer{send, receive, in, out} {
			if err := closer.Close(); err != nil {
				t.Error(err)
			}
		}
		select {
		case err := <-done:
			if err != nil && err != io.ErrClosedPipe {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("MCP server did not stop")
		}
	})
	w := &popupWire{t: t, encode: json.NewEncoder(send), frames: frames, errors: errors}
	caps := map[string]any{}
	if capable {
		caps["elicitation"] = map[string]any{"form": map[string]any{}}
	}
	w.send(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"capabilities": caps}})
	if f := w.read(); string(f.ID) != "1" || len(f.Error) != 0 {
		t.Fatalf("initialize: %+v", f)
	}
	return w
}

func (w *popupWire) send(v map[string]any) {
	w.t.Helper()
	v["jsonrpc"] = "2.0"
	if err := w.encode.Encode(v); err != nil {
		w.t.Fatal(err)
	}
}

func (w *popupWire) read() popupFrame {
	w.t.Helper()
	select {
	case f, ok := <-w.frames:
		if !ok {
			w.t.Fatalf("MCP output closed: %v", <-w.errors)
		}
		return f
	case <-time.After(10 * time.Second):
		w.t.Fatal("MCP response timed out")
		return popupFrame{}
	}
}

func (w *popupWire) call(args map[string]any) {
	w.t.Helper()
	w.send(map[string]any{"id": 2, "method": "tools/call", "params": map[string]any{"name": "write_memory", "arguments": args}})
}

func (w *popupWire) result() map[string]any {
	w.t.Helper()
	f := w.read()
	if f.Method == "notifications/cancelled" {
		f = w.read()
	}
	if f.Method != "" || string(f.ID) != "2" || len(f.Error) != 0 || f.Result.IsError || f.Result.Content == nil {
		w.t.Fatalf("tools/call: %+v", f)
	}
	// A subsequent response proves no extra elicitation was queued.
	w.send(map[string]any{"id": 3, "method": "ping"})
	if next := w.read(); next.Method != "" || string(next.ID) != "3" {
		w.t.Fatalf("unexpected extra request: %+v", next)
	}
	return f.Result.Content
}

func popupFixture(t *testing.T) Config {
	t.Helper()
	withTempHome(t)
	cfg := mustConfig(t)
	cfg.MCPWritePolicy = mcpWritePolicyPropose
	for _, path := range []string{memoriesRoot(cfg), sourcesRoot(cfg), cfg.DataDir, cfg.StateDir} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeConfig(cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func popupArgs() map[string]any {
	return map[string]any{"scope": "project:demo", "type": "fact", "title": "A quoted \"title\"\u202e", "text": "Project uses Go.", "approved": true, "popup": false}
}

func popupCounts(t *testing.T, cfg Config, memories, pending int) {
	t.Helper()
	files, err := allMemoryFiles(cfg)
	if err != nil || len(files) != memories {
		t.Fatalf("memories=%v, want %d; err=%v", files, memories, err)
	}
	proposals, err := listMCPWriteProposals(cfg)
	if err != nil || len(proposals) != pending {
		t.Fatalf("pending=%v, want %d; err=%v", proposals, pending, err)
	}
}

func TestPopupConfig(t *testing.T) {
	// AC-001.1, AC-001.2: strict values, default on, and not Write-owned.
	for _, tc := range []struct {
		value string
		off   bool
		bad   bool
	}{{"", false, false}, {"true", false, false}, {"false", true, false}, {`"maybe"`, false, true}, {"1", false, true}, {`""`, false, true}} {
		t.Run(tc.value, func(t *testing.T) {
			withTempHome(t)
			cfg := mustConfig(t)
			if err := os.MkdirAll(cfg.ConfigDir, 0o700); err != nil {
				t.Fatal(err)
			}
			body := ""
			if tc.value != "" {
				body = "mcp_approval_popup = " + tc.value + "\n"
			}
			path := filepath.Join(cfg.ConfigDir, "config.toml")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			loaded, err := loadConfigFor(testCtx(t))
			if tc.bad {
				if err == nil || !strings.Contains(err.Error(), "mcp_approval_popup") {
					t.Fatalf("invalid config error: %v", err)
				}
				return
			}
			if err != nil || loaded.MCPApprovalPopupOff != tc.off {
				t.Fatalf("off=%v err=%v", loaded.MCPApprovalPopupOff, err)
			}
			loaded.MCPApprovalPopupOff = !tc.off
			if err := writeConfig(loaded); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			popupLines := ""
			for _, line := range strings.Split(string(raw), "\n") {
				if strings.HasPrefix(line, "mcp_approval_popup") {
					popupLines += line + "\n"
				}
			}
			if popupLines != body {
				t.Fatalf("Write changed popup setting: %q, want %q", popupLines, body)
			}
		})
	}
}

func TestPopupAccept(t *testing.T) {
	// AC-001.3, AC-002.1: one request, bounded quoted data, durable approval.
	cfg := popupFixture(t)
	w := startPopupWire(t, testCtx(t), true)
	args := popupArgs()
	args["title"] = "Price " + args["title"].(string)
	args["text"] = strings.Repeat("界", 2000)
	w.call(args)
	f := w.read()
	if f.Method != "elicitation/create" {
		t.Fatalf("expected popup: %+v", f)
	}
	// Claude Code shows three message lines and one hint line, each cut at
	// the window width, so the message is three short lines and the counts
	// and flags ride in the field description.
	msg := f.Params.Message
	lines := strings.Split(msg, "\n")
	if len(lines) != 3 {
		t.Fatalf("message has %d lines, want 3: %s", len(lines), msg)
	}
	for i, line := range lines {
		if n := len([]rune(line)); n > 76 {
			t.Errorf("line %d has %d characters, want at most 76: %s", i+1, n, line)
		}
	}
	if want := `Save "Price A quoted \"title\"\u202e" as fact in project:demo?`; lines[0] != want {
		t.Errorf("line 1 = %s, want %s", lines[0], want)
	}
	if want := `"` + strings.Repeat("界", 72) + `"`; lines[1] != want || lines[2] != want {
		t.Errorf("text lines = %s / %s", lines[1], lines[2])
	}
	if strings.Contains(msg, "\u202e") {
		t.Fatalf("raw bidi character in message: %s", msg)
	}
	save := f.Params.Schema.Properties["save"]
	if save.Type != "boolean" || save.Title != "Save this agent note?" || save.Default || !reflect.DeepEqual(f.Params.Schema.Required, []string{"save"}) {
		t.Fatalf("schema=%+v", f.Params.Schema)
	}
	if want := "flags: money. Tick + Accept saves all 2000 characters (144 shown). Else it waits."; save.Hint != want {
		t.Errorf("hint = %q, want %q", save.Hint, want)
	}
	w.send(map[string]any{"id": f.ID, "result": map[string]any{"action": "accept", "content": map[string]any{"save": true}}})
	result := w.result()
	p := result["proposal"].(map[string]any)
	id, _ := p["id"].(string)
	memoryID, _ := p["memory_id"].(string)
	if p["status"] != "approved" || id == "" || memoryID == "" {
		t.Fatalf("result=%v", result)
	}
	popupCounts(t, cfg, 1, 0)
	d, err := readMCPProposalDecision(cfg, id)
	if err != nil || d == nil || d.DecidedVia != "popup" || d.Status != "approved" || d.MemoryID != memoryID {
		t.Fatalf("decision=%+v err=%v", d, err)
	}
	m, err := findMemory(cfg, memoryID)
	if err != nil || m.Text != args["text"] {
		t.Fatalf("saved text differs: err=%v", err)
	}
}

func TestPopupNotApproved(t *testing.T) {
	// AC-002.2: caller-controlled approved/popup arguments never approve or skip.
	for _, tc := range []struct{ name, reply, why string }{
		{"decline", `{"action":"decline","content":{"save":true}}`, "the user declined"},
		{"cancel", `{"action":"cancel","content":{"save":true}}`, "the user closed the popup"},
		{"false", `{"action":"accept","content":{"save":false}}`, "the box was not ticked"},
		{"string", `{"action":"accept","content":{"save":"true"}}`, "the box was not ticked"},
		{"missing", `{"action":"accept","content":{}}`, "the box was not ticked"},
		{"unknown action", `{"action":"Accept","content":{"save":true}}`, "the answer was not a clear yes"},
		{"error", "", "the client could not show or return the popup"},
		{"timeout", "", "no answer in 20ms"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := popupFixture(t)
			if tc.name == "timeout" {
				old := mcpPopupTimeout
				mcpPopupTimeout = 20 * time.Millisecond
				t.Cleanup(func() { mcpPopupTimeout = old })
			}
			w := startPopupWire(t, testCtx(t), true)
			w.call(popupArgs())
			f := w.read()
			if f.Method != "elicitation/create" {
				t.Fatalf("expected popup: %+v", f)
			}
			switch tc.name {
			case "timeout":
			case "error":
				w.send(map[string]any{"id": f.ID, "error": map[string]any{"code": -32603, "message": "client error"}})
			default:
				w.send(map[string]any{"id": f.ID, "result": json.RawMessage(tc.reply)})
			}
			result := w.result()
			if result["popup"] != popupWaiting(tc.why) {
				t.Fatalf("result=%v", result)
			}
			popupCounts(t, cfg, 0, 1)
		})
	}
}

func TestPopupEligibility(t *testing.T) {
	// AC-001.4, AC-001.5: a control asks; excluded calls leave pending unchanged.
	for _, mode := range []string{"control", "no capability", "off", "tentative", "credential", "profile"} {
		t.Run(mode, func(t *testing.T) {
			cfg := popupFixture(t)
			ctx := testCtx(t)
			args := popupArgs()
			switch mode {
			case "off":
				path := filepath.Join(cfg.ConfigDir, "config.toml")
				if err := os.WriteFile(path, []byte("mcp_write_policy = \"propose\"\nmcp_approval_popup = false\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "tentative":
				cfg.MCPWritePolicy = "tentative"
				if err := writeConfig(cfg); err != nil {
					t.Fatal(err)
				}
				args["text"] = "The price is $20."
			case "credential":
				args["text"] = "password=hidden-value"
			case "profile":
				ctx = withAgentProfile(ctx, agentProfile{Name: "popup-test", ReadScopes: []string{"project:demo"}, Write: mcpWritePolicyPropose, Tools: []string{"write_memory"}})
			}
			w := startPopupWire(t, ctx, mode != "no capability")
			w.call(args)
			if mode == "control" {
				f := w.read()
				if f.Method != "elicitation/create" {
					t.Fatalf("control did not ask: %+v", f)
				}
				w.send(map[string]any{"id": f.ID, "result": map[string]any{"action": "decline"}})
			}
			result := w.result()
			if mode == "credential" {
				if result["popup"] != "skipped: credential-like text" {
					t.Fatalf("result=%v", result)
				}
			} else if mode != "control" && result["popup"] != nil {
				t.Fatalf("held result changed: %v", result)
			}
			popupCounts(t, cfg, 0, 1)
		})
	}
}

func TestPopupRevalidatesBeforeApproval(t *testing.T) {
	// AC-002.3, AC-002.4: a stale answer cannot bypass config or digest checks.
	for _, mode := range []string{"policy", "off", "load error", "changed proposal"} {
		t.Run(mode, func(t *testing.T) {
			cfg := popupFixture(t)
			w := startPopupWire(t, testCtx(t), true)
			w.call(popupArgs())
			f := w.read()
			if f.Method != "elicitation/create" {
				t.Fatalf("expected popup: %+v", f)
			}
			configText := "mcp_write_policy = \"propose\"\n"
			switch mode {
			case "policy":
				configText = "mcp_write_policy = \"open\"\n"
			case "off":
				configText += "mcp_approval_popup = false\n"
			case "load error":
				configText += "mcp_approval_popup = \"maybe\"\n"
			case "changed proposal":
				proposals, err := listMCPWriteProposals(cfg)
				if err != nil || len(proposals) != 1 {
					t.Fatalf("proposals=%v err=%v", proposals, err)
				}
				proposals[0].Arguments["text"] = "Changed after display"
				if _, err := mcppkg.SaveProposal(cfg, proposals[0]); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(cfg.ConfigDir, "config.toml"), []byte(configText), 0o600); err != nil {
				t.Fatal(err)
			}
			w.send(map[string]any{"id": f.ID, "result": map[string]any{"action": "accept", "content": map[string]any{"save": true}}})
			result := w.result()
			want := popupWaiting("the write policy changed while the popup was open")
			if mode == "changed proposal" {
				want = "approval failed: proposal arguments changed; refusing decision"
			}
			if result["popup"] != want {
				t.Fatalf("result=%v", result)
			}
			popupCounts(t, cfg, 0, 1)
		})
	}
}

func popupWaiting(why string) string {
	return "not saved: " + why + ". The note waits for the owner (`mora mcp proposals list`). Tell the user. Do not send this same note again; other notes are fine."
}

func TestPopupFitNeverSplitsAnEscape(t *testing.T) {
	// A line must not end inside an escape, and the shown count must be exact.
	r := []rune("ab\u202ecd\n")
	for max, want := range map[int]int{0: 0, 2: 2, 7: 2, 8: 3, 10: 5, 11: 5, 12: 6, 99: 6} {
		if got := popupFit(r, max); got != want {
			t.Errorf("popupFit(max=%d) = %d, want %d", max, got, want)
		}
	}
}
