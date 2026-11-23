package mora

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	mcppkg "github.com/pyranthus-hq/mora/internal/mcp"
	"github.com/pyranthus-hq/mora/internal/memory"
)

// fakeMCPClient drives the real stdio server (serveMCP) the way a chat client
// does: it sends requests, and it answers the server's own requests, such as
// elicitation/create, with whatever a test scripts.
type fakeMCPClient struct {
	t      *testing.T
	w      *io.PipeWriter
	frames chan map[string]json.RawMessage
	done   chan error
	init   map[string]json.RawMessage
}

// startFakeMCPClient serves one session and sends initialize with caps as the
// client's declared capabilities.
func startFakeMCPClient(t *testing.T, caps string) *fakeMCPClient {
	t.Helper()
	serverIn, clientOut := io.Pipe()
	clientIn, serverOut := io.Pipe()
	c := &fakeMCPClient{t: t, w: clientOut, frames: make(chan map[string]json.RawMessage, 16), done: make(chan error, 1)}
	go func() {
		err := serveMCP(testCtx(t), serverOut, serverIn)
		serverOut.Close()
		c.done <- err
	}()
	go func() {
		sc := bufio.NewScanner(clientIn)
		sc.Buffer(make([]byte, 64*1024), 8<<20)
		for sc.Scan() {
			var f map[string]json.RawMessage
			if json.Unmarshal(sc.Bytes(), &f) == nil {
				c.frames <- f
			}
		}
		close(c.frames)
	}()
	t.Cleanup(func() {
		clientOut.Close()
		select {
		case <-c.done:
		case <-time.After(10 * time.Second):
			t.Error("MCP server did not stop")
		}
	})
	c.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": json.RawMessage(`{"protocolVersion":"2025-06-18","capabilities":` + caps + `,"clientInfo":{"name":"fake"}}`)})
	c.init = c.next()
	return c
}

func (c *fakeMCPClient) send(v any) {
	c.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		c.t.Fatal(err)
	}
	if _, err := c.w.Write(append(b, '\n')); err != nil {
		c.t.Fatal(err)
	}
}

func (c *fakeMCPClient) next() map[string]json.RawMessage {
	c.t.Helper()
	select {
	case f, ok := <-c.frames:
		if !ok {
			c.t.Fatal("server closed its output")
		}
		return f
	case <-time.After(15 * time.Second):
		c.t.Fatal("no frame from the server")
	}
	return nil
}

func (c *fakeMCPClient) instructions() string {
	c.t.Helper()
	var r struct {
		Instructions string `json:"instructions"`
	}
	if err := json.Unmarshal(c.init["result"], &r); err != nil {
		c.t.Fatalf("initialize result: %s", c.init)
	}
	return r.Instructions
}

// toolResult is one tools/call outcome as the agent sees it.
type toolResult struct {
	text    string
	isError bool
	data    map[string]any
}

// call sends one tools/call. answer scripts the client's reply to each
// elicitation the server sends while the call runs: a result object, an
// error object under the key "error", or nil to never answer.
func (c *fakeMCPClient) call(id int, name string, args map[string]any, answer func(params map[string]any) map[string]any) (toolResult, []map[string]any) {
	c.t.Helper()
	c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	var asked []map[string]any
	for {
		f := c.next()
		if method, ok := f["method"]; ok {
			var m string
			_ = json.Unmarshal(method, &m)
			if m != "elicitation/create" {
				continue // notifications/cancelled after a timeout
			}
			var params map[string]any
			if err := json.Unmarshal(f["params"], &params); err != nil {
				c.t.Fatal(err)
			}
			asked = append(asked, params)
			reply := answer(params)
			if reply == nil {
				continue
			}
			frame := map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(f["id"])}
			if e, ok := reply["error"]; ok {
				frame["error"] = e
			} else {
				frame["result"] = reply
			}
			c.send(frame)
			continue
		}
		if string(f["id"]) != fmt.Sprint(id) {
			c.t.Fatalf("unexpected frame %s", f)
		}
		var r struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError           bool           `json:"isError"`
			StructuredContent map[string]any `json:"structuredContent"`
		}
		if err := json.Unmarshal(f["result"], &r); err != nil || len(r.Content) == 0 {
			c.t.Fatalf("tools/call result: %s", f)
		}
		return toolResult{text: r.Content[0].Text, isError: r.IsError, data: r.StructuredContent}, asked
	}
}

func answerWith(result map[string]any) func(map[string]any) map[string]any {
	return func(map[string]any) map[string]any { return result }
}

const elicitCaps = `{"elicitation":{"form":{}}}`

// seedAgentNote writes an agent note straight to the vault: tentative when
// asked, as a tentative-policy write_memory would leave it.
func seedAgentNote(t *testing.T, cfg Config, id, title, text string, tentative bool) Memory {
	t.Helper()
	m := Memory{ID: id, Scope: "project:heron", Type: "decision", Title: title, Source: "mcp", CreatedAt: "2026-11-01T10:00:00Z", Text: text}
	if tentative {
		m.Tags = []string{memory.TentativeTag}
	}
	if err := writeMemory(cfg, m); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
	return m
}

func reparse(t *testing.T, cfg Config, id string) Memory {
	t.Helper()
	found, err := findMemory(cfg, id)
	if err != nil {
		t.Fatal(err)
	}
	m, err := parseMemory(found.Path)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func anyOwnerConfirmed(t *testing.T, cfg Config) []string {
	t.Helper()
	files, err := allMemoryFiles(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, p := range files {
		if m, err := parseMemory(p); err == nil && memory.IsOwnerConfirmed(m) {
			ids = append(ids, m.ID)
		}
	}
	return ids
}

// Every answer but an accept with an explicit yes leaves the note tentative and
// tells the agent why in one line. Only accept+true makes it owner-confirmed.
func TestMCPConfirmMemoryElicitationOutcomes(t *testing.T) {
	cases := []struct {
		name    string
		caps    string
		answer  func(map[string]any) map[string]any
		confirm bool
		asks    int
		reason  string
	}{
		{"accept", elicitCaps, answerWith(map[string]any{"action": "accept", "content": map[string]any{"confirm": true}}), true, 1, ""},
		{"accept-unticked", elicitCaps, answerWith(map[string]any{"action": "accept", "content": map[string]any{"confirm": false}}), false, 1, "did not tick yes"},
		{"accept-string-true", elicitCaps, answerWith(map[string]any{"action": "accept", "content": map[string]any{"confirm": "true"}}), false, 1, "did not tick yes"},
		{"accept-no-content", elicitCaps, answerWith(map[string]any{"action": "accept"}), false, 1, "did not tick yes"},
		{"decline", elicitCaps, answerWith(map[string]any{"action": "decline"}), false, 1, "came back declined"},
		{"cancel", elicitCaps, answerWith(map[string]any{"action": "cancel"}), false, 1, "dismissed or cancelled"},
		{"unknown-action", elicitCaps, answerWith(map[string]any{"action": "yes", "content": map[string]any{"confirm": true}}), false, 1, "dismissed or cancelled"},
		{"client-error", elicitCaps, answerWith(map[string]any{"error": map[string]any{"code": -32601, "message": "method not found"}}), false, 1, "the prompt failed"},
		{"unreadable", elicitCaps, answerWith(map[string]any{"action": 7}), false, 1, "not readable"},
		{"timeout", elicitCaps, func(map[string]any) map[string]any { return nil }, false, 1, "nobody answered"},
		{"no-capability", `{}`, answerWith(map[string]any{"action": "accept", "content": map[string]any{"confirm": true}}), false, 0, "did not declare MCP elicitation"},
		{"url-only", `{"elicitation":{"url":{}}}`, answerWith(map[string]any{"action": "accept", "content": map[string]any{"confirm": true}}), false, 0, "did not declare MCP elicitation"},
	}
	for _, tc := range cases {
		subRun(t, tc.name, func(t *testing.T) {
			withTempHome(t)
			run(t, "init")
			run(t, "config", "mcp-write-policy", mcppkg.WritePolicyTentative)
			cfg := mustConfig(t)
			orig := ownerConfirmElicitTimeout
			ownerConfirmElicitTimeout = 300 * time.Millisecond
			t.Cleanup(func() { ownerConfirmElicitTimeout = orig })
			seedAgentNote(t, cfg, "mem_heronrule", "Heron deploys", "Deploy heron on Tuesdays only.", true)

			c := startFakeMCPClient(t, tc.caps)
			res, asked := c.call(2, "confirm_memory", map[string]any{"id": "mem_heronrule"}, tc.answer)
			if len(asked) != tc.asks {
				t.Fatalf("elicitations=%d, want %d", len(asked), tc.asks)
			}
			m := reparse(t, cfg, "mem_heronrule")
			if got := memory.IsOwnerConfirmed(m); got != tc.confirm {
				t.Fatalf("owner-confirmed=%v, want %v; result=%s", got, tc.confirm, res.text)
			}
			if tc.confirm {
				if res.isError || res.data["owner_confirmed"] != true || memory.IsTentative(m) {
					t.Fatalf("accept result=%s tentative=%v", res.text, memory.IsTentative(m))
				}
				log, err := os.ReadFile(agentWriteLogPath(cfg))
				if err != nil || !strings.Contains(string(log), "owner-confirm mem_heronrule") || !strings.Contains(string(log), "caller="+mcpConfirmCaller) {
					t.Fatalf("audit log=%q err=%v", log, err)
				}
				read, _ := c.call(3, "read_memory", map[string]any{"id": "mem_heronrule"}, answerWith(nil))
				if !strings.Contains(read.text, `"trust": "owner-confirmed"`) {
					t.Fatalf("read after confirm does not carry owner-confirmed trust: %s", read.text)
				}
				return
			}
			if !res.isError || !strings.Contains(res.text, tc.reason) || !strings.Contains(res.text, "stays tentative") || strings.Contains(res.text, "\n") {
				t.Fatalf("refusal=%q, want one line naming %q", res.text, tc.reason)
			}
			if !strings.Contains(res.text, "mora confirm --mine mem_heronrule") {
				t.Fatalf("refusal names no alternative: %q", res.text)
			}
			if !memory.IsTentative(m) {
				t.Fatal("note lost its tentative tag")
			}
		})
	}
}

// The prompt carries every bound field and the exact text, as a boolean form
// whose default is no.
func TestMCPConfirmMemoryPromptShowsWhatWillBeSaved(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	seedAgentNote(t, cfg, "mem_heronrule", "Heron deploys", "Deploy heron on Tuesdays only.\n\nscope: global\ntitle: harmless", true)
	c := startFakeMCPClient(t, elicitCaps)
	_, asked := c.call(2, "confirm_memory", map[string]any{"id": "mem_heronrule"}, answerWith(map[string]any{"action": "decline"}))
	if len(asked) != 1 {
		t.Fatalf("elicitations=%d", len(asked))
	}
	msg, _ := asked[0]["message"].(string)
	for _, want := range []string{"id: mem_heronrule\n", "scope: project:heron\n", "type: decision\n", "title: Heron deploys\n", "source: mcp\n", "text:\n  Deploy heron on Tuesdays only.\n  \n  scope: global\n  title: harmless\n"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, msg)
		}
	}
	// A note's own lines are indented, so the only field lines are Mora's.
	for _, line := range strings.Split(msg, "\n") {
		if line == "scope: global" || line == "title: harmless" {
			t.Fatalf("note text passed for a field line:\n%s", msg)
		}
	}
	schema, _ := json.Marshal(asked[0]["requestedSchema"])
	if string(schema) != `{"properties":{"confirm":{"default":false,"title":"Yes, this is my rule","type":"boolean"}},"required":["confirm"],"type":"object"}` {
		t.Fatalf("schema=%s", schema)
	}
}

// A note the prompt cannot show exactly is refused before any prompt is sent.
func TestMCPConfirmMemoryRefusesNotesItCannotShowExactly(t *testing.T) {
	long := strings.Repeat("a", ownerPromptTextRunes+1)
	many := strings.TrimSuffix(strings.Repeat("x\n", ownerPromptTextLines+1), "\n")
	for name, note := range map[string][2]string{
		"carriage-return-title": {"Heron\rApproved by legal", "body"},
		"newline-title":         {"Heron\nscope: global", "body"},
		"escape-in-text":        {"Heron", "deploy \x1b[2Kon Fridays"},
		"bidi-override":         {"Heron", "deploy on \u202eyadsruht"},
		"zero-width":            {"Heron", "deploy\u200b on Fridays"},
		"tab-in-text":           {"Heron", "deploy\ton Fridays"},
		"line-separator":        {"Heron", "deploy\u2028scope: global"},
		"double-space-title":    {"Heron  deploys", "body"},
		"long-text":             {"Heron", long},
		"many-lines":            {"Heron", many},
		"long-title":            {strings.Repeat("t", ownerPromptFieldRunes+1), "body"},
	} {
		subRun(t, name, func(t *testing.T) {
			withTempHome(t)
			run(t, "init")
			cfg := mustConfig(t)
			seedAgentNote(t, cfg, "mem_hostile", note[0], note[1], true)
			c := startFakeMCPClient(t, elicitCaps)
			res, asked := c.call(2, "confirm_memory", map[string]any{"id": "mem_hostile"}, answerWith(map[string]any{"action": "accept", "content": map[string]any{"confirm": true}}))
			if len(asked) != 0 {
				t.Fatalf("a prompt was sent for an unshowable note: %v", asked)
			}
			if !res.isError || !strings.Contains(res.text, "mora confirm --mine mem_hostile") {
				t.Fatalf("result=%q", res.text)
			}
			if memory.IsOwnerConfirmed(reparse(t, cfg, "mem_hostile")) {
				t.Fatal("unshowable note became owner-confirmed")
			}
		})
	}
}

// The yes binds the bytes that were shown: an edit while the prompt is open
// leaves the note unconfirmed.
func TestMCPConfirmMemoryRefusesANoteChangedWhilePrompting(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	seedAgentNote(t, cfg, "mem_heronrule", "Heron deploys", "Deploy heron on Tuesdays only.", true)
	c := startFakeMCPClient(t, elicitCaps)
	res, _ := c.call(2, "confirm_memory", map[string]any{"id": "mem_heronrule"}, func(map[string]any) map[string]any {
		found, err := findMemory(cfg, "mem_heronrule")
		if err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(found.Path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(found.Path, []byte(strings.Replace(string(raw), "Tuesdays", "any day", 1)), 0o644); err != nil {
			t.Fatal(err)
		}
		return map[string]any{"action": "accept", "content": map[string]any{"confirm": true}}
	})
	if !res.isError || !strings.Contains(res.text, "changed after it was shown") {
		t.Fatalf("result=%q", res.text)
	}
	if ids := anyOwnerConfirmed(t, cfg); len(ids) != 0 {
		t.Fatalf("owner-confirmed after an edit: %v", ids)
	}
}

// No MCP path yields an owner-confirmed note without an accept. Every tool in
// the catalog is driven under every write policy, with arguments aimed at the
// tentative note and at the reserved tag, while the client answers every
// prompt with something other than an explicit yes.
func TestNoMCPPathOwnerConfirmsWithoutAccept(t *testing.T) {
	noAnswers := []map[string]any{
		{"action": "decline"},
		{"action": "cancel"},
		{"action": "accept", "content": map[string]any{"confirm": false}},
		{"action": "accept", "content": map[string]any{"confirm": "yes"}},
		{"action": "accept", "content": map[string]any{"Confirm": true}},
		{"error": map[string]any{"code": -1, "message": "no"}},
	}
	for _, policy := range []string{mcppkg.WritePolicyOpen, mcppkg.WritePolicyTentative, mcppkg.WritePolicyPropose, mcppkg.WritePolicyReadonly} {
		subRun(t, policy, func(t *testing.T) {
			withTempHome(t)
			run(t, "init")
			run(t, "config", "mcp-write-policy", policy)
			cfg := mustConfig(t)
			seed := seedAgentNote(t, cfg, "mem_heronrule", "Heron deploys", "Deploy heron on Tuesdays only.", true)
			tag := memory.OwnerConfirmedTagPrefix + memory.OwnerConfirmedDigest(seed)
			c := startFakeMCPClient(t, elicitCaps)
			id, n := 2, 0
			answer := func(map[string]any) map[string]any { n++; return noAnswers[n%len(noAnswers)] }
			confirmEach := func() {
				for range noAnswers {
					c.call(id, "confirm_memory", map[string]any{"id": "mem_heronrule"}, answer)
					id++
				}
			}
			// Before any other tool, and checked at once, so delete_memory
			// under open cannot remove a wrongly confirmed note unseen.
			confirmEach()
			if ids := anyOwnerConfirmed(t, cfg); len(ids) != 0 {
				t.Fatalf("owner-confirmed without an accept under %s: %v", policy, ids)
			}
			for _, def := range mcppkg.ToolCatalog() {
				for _, args := range []map[string]any{
					{"id": "mem_heronrule", "title": "Heron deploys", "text": "Deploy heron on Tuesdays only.", "query": "heron", "name": "heron", "start": "2026-01-01", "end": "2026-01-02"},
					{"id": "mem_heronrule", "title": "Mine", "text": "owner rule", "tags": []string{tag}, "source": "owner-confirmed", "target": "mem_heronrule", "disposition": "keep"},
				} {
					c.call(id, def.Name, args, answer)
					id++
				}
			}
			confirmEach()
			if policy != mcppkg.WritePolicyReadonly && n == 0 {
				t.Fatal("no prompt was ever sent, so the no-answers were never exercised")
			}
			if ids := anyOwnerConfirmed(t, cfg); len(ids) != 0 {
				t.Fatalf("owner-confirmed without an accept under %s: %v", policy, ids)
			}
		})
	}
}

// The init text and dispatch agree on confirm_memory under every policy:
// readonly says it refuses and refuses before any prompt; every other policy
// advertises it and confirms on an accept.
func TestMCPConfirmMemoryPolicyAgreement(t *testing.T) {
	for _, policy := range []string{mcppkg.WritePolicyOpen, mcppkg.WritePolicyTentative, mcppkg.WritePolicyPropose, mcppkg.WritePolicyReadonly} {
		subRun(t, policy, func(t *testing.T) {
			withTempHome(t)
			run(t, "init")
			run(t, "config", "mcp-write-policy", policy)
			cfg := mustConfig(t)
			seedAgentNote(t, cfg, "mem_heronrule", "Heron deploys", "Deploy heron on Tuesdays only.", true)
			c := startFakeMCPClient(t, elicitCaps)
			instr := c.instructions()
			action, _ := mcppkg.MutationAction(policy, "confirm_memory")
			res, asked := c.call(2, "confirm_memory", map[string]any{"id": "mem_heronrule"}, answerWith(map[string]any{"action": "accept", "content": map[string]any{"confirm": true}}))
			confirmed := memory.IsOwnerConfirmed(reparse(t, cfg, "mem_heronrule"))
			if policy == mcppkg.WritePolicyReadonly {
				if action != mcppkg.ActionRefuse || !strings.Contains(instr, "confirm_memory; they all refuse") || strings.Contains(instr, "call confirm_memory with its id") {
					t.Fatalf("readonly init/dispatch disagree: action=%s", action)
				}
				if len(asked) != 0 || !res.isError || confirmed {
					t.Fatalf("readonly confirm: asked=%d result=%q confirmed=%v", len(asked), res.text, confirmed)
				}
				return
			}
			if action != mcppkg.ActionExecute || !strings.Contains(instr, "call confirm_memory with its id") {
				t.Fatalf("%s init/dispatch disagree: action=%s", policy, action)
			}
			if policy == mcppkg.WritePolicyPropose && !strings.Contains(instr, "a note already in the vault") {
				t.Fatal("propose init text promises write_memory then confirm_memory, but write_memory only queues a proposal")
			}
			if len(asked) != 1 || res.isError || !confirmed {
				t.Fatalf("%s confirm: asked=%d result=%q confirmed=%v", policy, len(asked), res.text, confirmed)
			}
		})
	}
}

// "Remember this as my rule" end to end under the default policy: write_memory
// saves a tentative note, confirm_memory asks, and the yes makes it the
// owner's.
func TestMCPRememberAsMyRuleFlow(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	run(t, "config", "mcp-write-policy", mcppkg.WritePolicyTentative)
	cfg := mustConfig(t)
	c := startFakeMCPClient(t, elicitCaps)
	w, _ := c.call(2, "write_memory", map[string]any{"title": "Heron deploys", "text": "Deploy heron on Tuesdays only.", "type": "decision", "scope": "project:heron"}, answerWith(nil))
	mem, _ := w.data["memory"].(map[string]any)
	id, _ := mem["id"].(string)
	if w.isError || id == "" || !memory.IsTentative(reparse(t, cfg, id)) {
		t.Fatalf("write=%s", w.text)
	}
	res, asked := c.call(3, "confirm_memory", map[string]any{"id": id}, answerWith(map[string]any{"action": "accept", "content": map[string]any{"confirm": true}}))
	if len(asked) != 1 || res.isError || !memory.IsOwnerConfirmed(reparse(t, cfg, id)) {
		t.Fatalf("confirm=%s", res.text)
	}
	again, asked := c.call(4, "confirm_memory", map[string]any{"id": id}, answerWith(nil))
	if len(asked) != 0 || again.isError || again.data["changed"] != false {
		t.Fatalf("second confirm=%s", again.text)
	}
}
