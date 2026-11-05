package eywa

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeService is an in-process stand-in for the agent MCP API: an OAuth2
// client_credentials token endpoint plus a streamable HTTP MCP endpoint that
// publishes its tools behind a gateway-style "<target>___" prefix.
type fakeService struct {
	t   *testing.T
	srv *httptest.Server

	mu         sync.Mutex
	sse        bool
	tokens     int
	tokenCode  int // non-zero: the token endpoint answers this status
	mcpStatus  map[string]int
	retryAfter string
	rpcErr     map[string]rpcErrorBody // tool -> one-shot JSON-RPC error
	toolErr    map[string]string       // tool -> one-shot tool error code
	calls      []toolCall
	pages      []fakePage
	facts      map[string]string // fact_id -> slot key
	slots      map[string][]map[string]any
	writes     map[string]map[string]any
	writeSeq   int
	remembered []map[string]any
	session    string
}

type fakePage struct {
	items   []map[string]any
	next    string
	hasMore bool
}

type toolCall struct {
	Name string
	Args map[string]any
}

func newFakeService(t *testing.T) *fakeService {
	f := &fakeService{t: t, mcpStatus: map[string]int{}, rpcErr: map[string]rpcErrorBody{}, toolErr: map[string]string{},
		facts: map[string]string{}, slots: map[string][]map[string]any{}, writes: map[string]map[string]any{}, session: "sess-1"}
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2/token", f.token)
	mux.HandleFunc("/mcp", f.mcp)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeService) config() Config {
	return Config{Endpoint: f.srv.URL + "/mcp", TokenURL: f.srv.URL + "/oauth2/token", ClientID: "client-a",
		ClientSecret: "secret-a", OAuthScope: "memory/agent", AgentID: "agt_TEST", Topic: "payments"}
}

func (f *fakeService) client(t *testing.T) *Client {
	t.Helper()
	c, err := NewClient(f.config(), ClientOptions{HTTP: f.srv.Client(), Sleep: func(context.Context, time.Duration) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (f *fakeService) token(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tokenCode != 0 {
		w.WriteHeader(f.tokenCode)
		_, _ = w.Write([]byte(`{"error":"invalid_client"}`))
		return
	}
	if err := r.ParseForm(); err != nil || r.PostForm.Get("grant_type") != "client_credentials" || r.PostForm.Get("scope") != "memory/agent" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("client-a:secret-a"))
	if r.Header.Get("Authorization") != want {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	f.tokens++
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": fmt.Sprintf("tok-%d", f.tokens), "token_type": "Bearer", "expires_in": 3600})
}

func (f *fakeService) reply(w http.ResponseWriter, msg map[string]any) {
	b, _ := json.Marshal(msg)
	if f.sse {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

func (f *fakeService) mcp(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer tok-") {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	tool := strings.TrimPrefix(req.Params.Name, "eywa-memory___")
	if code := f.mcpStatus[tool]; code != 0 && req.Method == "tools/call" {
		delete(f.mcpStatus, tool)
		if f.retryAfter != "" {
			w.Header().Set("Retry-After", f.retryAfter)
		}
		w.WriteHeader(code)
		return
	}
	switch req.Method {
	case "initialize":
		w.Header().Set("Mcp-Session-Id", f.session)
		f.reply(w, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "fake"}}})
		return
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if r.Header.Get("Mcp-Session-Id") != f.session {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	switch req.Method {
	case "tools/list":
		var tools []map[string]any
		for _, n := range []string{"remember", "ask", "get_memory", "history", "changes_since"} {
			tools = append(tools, map[string]any{"name": "eywa-memory___" + n})
		}
		f.reply(w, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"tools": tools}})
	case "tools/call":
		f.calls = append(f.calls, toolCall{Name: tool, Args: req.Params.Arguments})
		if e, ok := f.rpcErr[tool]; ok {
			delete(f.rpcErr, tool)
			f.reply(w, map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": e})
			return
		}
		if code, ok := f.toolErr[tool]; ok {
			delete(f.toolErr, tool)
			f.reply(w, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"content": []map[string]any{{"type": "text", "text": "refused"}}, "isError": true,
				"structuredContent": map[string]any{"error": map[string]any{"code": code, "message": "refused by the fake"}}}})
			return
		}
		out, rpcE := f.call(tool, req.Params.Arguments)
		if rpcE != nil {
			f.reply(w, map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": rpcE})
			return
		}
		text, _ := json.Marshal(out)
		// Mirror a gateway: the payload rides as text content (no structuredContent).
		f.reply(w, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"content": []map[string]any{{"type": "text", "text": string(text)}}}})
	default:
		f.reply(w, map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32601, "message": "method not found"}})
	}
}

var forbidden = &rpcErrorBody{Code: -32001, Message: "Forbidden"}

func (f *fakeService) call(tool string, a map[string]any) (any, *rpcErrorBody) {
	switch tool {
	case "changes_since":
		idx := 0
		if c, ok := a["cursor"].(string); ok {
			if !strings.HasPrefix(c, "c") {
				return nil, &rpcErrorBody{Code: -32602, Message: "Invalid params"}
			}
			if _, err := fmt.Sscanf(c, "c%d", &idx); err != nil {
				return nil, &rpcErrorBody{Code: -32602, Message: "Invalid params"}
			}
		} else if _, ok := a["since"].(string); !ok {
			return nil, &rpcErrorBody{Code: -32602, Message: "Invalid params"}
		}
		if idx >= len(f.pages) {
			return map[string]any{"items": []any{}, "next": fmt.Sprintf("c%d", len(f.pages)), "has_more": false}, nil
		}
		p := f.pages[idx]
		return map[string]any{"items": p.items, "next": p.next, "has_more": p.hasMore}, nil
	case "get_memory":
		if id, ok := a["write_id"].(string); ok {
			w, ok := f.writes[id]
			if !ok {
				return nil, forbidden
			}
			return map[string]any{"write": w}, nil
		}
		key := ""
		if id, ok := a["fact_id"].(string); ok {
			key = f.facts[id]
		} else {
			key = fmt.Sprintf("%v\x00%v", a["entity"], a["attribute"])
		}
		for _, v := range f.slots[key] {
			if v["status"] == "current" {
				return map[string]any{"version": v}, nil
			}
		}
		return nil, forbidden
	case "history":
		key := fmt.Sprintf("%v\x00%v", a["entity"], a["attribute"])
		var vs []map[string]any
		for _, v := range f.slots[key] {
			row := map[string]any{}
			for _, k := range []string{"version_id", "attribute", "value", "status", "valid_from", "valid_to", "recorded_at", "retired_at"} {
				row[k] = v[k]
			}
			row["source_uri"] = v["lineage"].(map[string]any)["source_uri"]
			vs = append(vs, row)
		}
		return map[string]any{"entity": a["entity"], "versions": vs}, nil
	case "remember":
		f.remembered = append(f.remembered, a)
		f.writeSeq++
		id := fmt.Sprintf("wrt_%02d", f.writeSeq)
		f.writes[id] = map[string]any{"write_id": id, "status": "in_review", "reason": nil}
		return map[string]any{"write_id": id, "status": "received"}, nil
	}
	return nil, &rpcErrorBody{Code: -32601, Message: "unknown tool"}
}

// addVersion registers one fact version under (entity, attribute).
func (f *fakeService) addVersion(factID, versionID, entity, attribute, value, status, recorded, retired string) {
	key := entity + "\x00" + attribute
	f.facts[factID] = key
	var retiredAt any
	if retired != "" {
		retiredAt = retired
	}
	f.slots[key] = append(f.slots[key], map[string]any{
		"version_id": versionID, "fact_id": factID, "entity": entity, "attribute": attribute, "value": value,
		"topic": "payments", "visibility": "tenant", "status": status, "valid_from": recorded, "valid_to": retiredAt,
		"recorded_at": recorded, "retired_at": retiredAt,
		"lineage": map[string]any{"source_uri": "https://wiki.example.com/" + versionID, "content_hash": strings.Repeat("a", 64), "signer": "usr_1", "excerpt": value + " per the wiki"},
	})
}

func (f *fakeService) retire(entity, attribute, versionID, at string) {
	for _, v := range f.slots[entity+"\x00"+attribute] {
		if v["version_id"] == versionID {
			v["status"], v["retired_at"], v["valid_to"] = "retired", at, at
		}
	}
}

func (f *fakeService) toolCalls(name string) []toolCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []toolCall
	for _, c := range f.calls {
		if c.Name == name {
			out = append(out, c)
		}
	}
	return out
}
