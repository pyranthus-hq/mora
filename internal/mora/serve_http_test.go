package mora

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestServeHTTPCallAllowlistBlocksDelete asserts the generic /call escape hatch
// refuses delete_memory (the only irreversibly destructive tool) with 403 BEFORE
// it ever reaches the dispatcher — so no vault is required to prove it. This is
// the least-privilege guard against a prompt-injected page driving an os.Remove
// through the token-holding AI browser.
func TestServeHTTPCallAllowlistBlocksDelete(t *testing.T) {
	s := &httpServer{token: "tok", port: 7777}
	h := s.hostGuard(s.auth(s.routes()))

	req := httptest.NewRequest(http.MethodPost, "/call",
		strings.NewReader(`{"name":"delete_memory","arguments":{"id":"x"}}`))
	req.Host = "127.0.0.1:7777"
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("delete_memory via /call: got HTTP %d, want 403\nbody: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "not permitted") {
		t.Errorf("expected a 'not permitted' error body, got: %s", rec.Body.String())
	}
}

// TestServeHTTPCallAllowlistShape pins the allowlist: delete_memory is excluded,
// and every other tool in callMCPTool's switch is reachable. If a new tool is
// added to the MCP dispatcher this test does NOT fail (it only checks the known
// set), but delete_memory must never appear here.
func TestServeHTTPCallAllowlistShape(t *testing.T) {
	if httpCallAllowed["delete_memory"] {
		t.Fatal("delete_memory must NOT be reachable via POST /call")
	}
	for _, tool := range []string{
		"brief", "context_memory", "digest", "get_entity", "list_entities",
		"list_memory", "meeting_prep", "read_memory", "search_memory", "think", "write_memory",
	} {
		if !httpCallAllowed[tool] {
			t.Errorf("expected %q to be reachable via POST /call", tool)
		}
	}
}

// TestServeHTTPPortGuardRejectsZero asserts the --port range guard fires before
// the listener binds. Port 0 is the trap: net.Listen would bind a random
// ephemeral port that the hostGuard allowlist (built from *port) can never match.
func TestServeHTTPPortGuardRejectsZero(t *testing.T) {
	t.Setenv("MORA_CONFIG_DIR", t.TempDir())
	var out strings.Builder
	err := serveLoopbackHTTP(context.Background(), []string{"--port", "0"}, &out)
	if err == nil {
		t.Fatal("serveLoopbackHTTP(--port 0): want error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid --port") {
		t.Fatalf("want an 'invalid --port' error, got: %v", err)
	}
}
