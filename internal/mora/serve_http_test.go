package mora

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	h := s.handler(testCtx(t))

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
	err := serveLoopbackHTTP(testCtx(t), []string{"--port", "0"}, &out, &out)
	if err == nil {
		t.Fatal("serveLoopbackHTTP(--port 0): want error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid --port") {
		t.Fatalf("want an 'invalid --port' error, got: %v", err)
	}
}

func TestServeHTTPTokenUpgrade(t *testing.T) {
	for _, mode := range []string{"legacy", "fresh", "scrub-failure"} {
		t.Run(mode, func(t *testing.T) {
			withTempHome(t)
			run(t, "init")
			cfg := mustConfig(t)
			old := strings.Repeat("a", 64)
			if mode != "fresh" {
				if err := os.WriteFile(filepath.Join(cfg.ConfigDir, "http.json"), []byte(`{"token":"`+old+`"}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			log := filepath.Join(cfg.StateDir, "serve-http.out.log")
			if err := os.MkdirAll(cfg.StateDir, 0700); err != nil {
				t.Fatal(err)
			}
			banner := "  token: " + old + "\n"
			if mode == "scrub-failure" {
				if err := os.Mkdir(log, 0700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(log, []byte("keep\n"+banner), 0640); err != nil {
				t.Fatal(err)
			}
			var out, warnings bytes.Buffer
			if err := cmdServe(testCtx(t), []string{"http", "--print-token"}, &out, &warnings); err != nil {
				t.Fatal(err)
			}
			token := strings.TrimSpace(out.String())
			if len(token) != 64 || token == old {
				t.Fatal("print-token did not return the new token alone")
			}
			var stored struct {
				Token   string `json:"token"`
				Version int    `json:"token_version"`
			}
			b, err := os.ReadFile(filepath.Join(cfg.ConfigDir, "http.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(b, &stored); err != nil {
				t.Fatal(err)
			}
			if stored.Token != token || stored.Version != 2 {
				t.Fatal("printed token differs from persisted version 2 token")
			}
			if strings.Contains(warnings.String(), token) || strings.Contains(warnings.String(), old) {
				t.Fatal("token leaked to stderr")
			}
			if strings.Contains(warnings.String(), "HTTP token rotated") != (mode != "fresh") {
				t.Fatalf("rotation notice: %s", &warnings)
			}
			if mode != "fresh" {
				for _, want := range []string{"Restart any already-running mora serve http", "still accepts the old token and rejects the new one", "mora serve http install", "systemctl --user restart mora-serve-http.service"} {
					if !strings.Contains(warnings.String(), want) {
						t.Fatalf("rotation notice missing %q: %s", want, &warnings)
					}
				}
			}
			if strings.Contains(warnings.String(), "warning:") != (mode == "scrub-failure") {
				t.Fatalf("scrub warning: %s", &warnings)
			}
			if mode != "scrub-failure" {
				b, err := os.ReadFile(log)
				if err != nil {
					t.Fatal(err)
				}
				want := "keep\n"
				if mode == "fresh" {
					want += banner
				}
				if string(b) != want {
					t.Fatalf("log=%q, want %q", b, want)
				}
				// A versioned start must not scrub again, even if a banner is reintroduced.
				if err := os.WriteFile(log, []byte(banner), 0640); err != nil {
					t.Fatal(err)
				}
			}
			out.Reset()
			warnings.Reset()
			if err := cmdServe(testCtx(t), []string{"http", "--print-token"}, &out, &warnings); err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(out.String()) != token || warnings.Len() != 0 {
				t.Fatal("second start rotated, warned, or changed output")
			}
			if mode != "scrub-failure" {
				b, err := os.ReadFile(log)
				if err != nil {
					t.Fatal(err)
				}
				if string(b) != banner {
					t.Fatal("non-rotating start scrubbed log")
				}
			}
		})
	}
}
