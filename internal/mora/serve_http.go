package mora

// Loopback HTTP interface for sandboxed AI browsers (issue #54).
//
// WHY THIS EXISTS
// ---------------
// mora's primary agent transport is `mora mcp serve` — a line-oriented stdio
// JSON-RPC server. That works for local hosts (Claude Code, Codex) that can
// SPAWN the process and pipe stdin/stdout. It does NOT work for a sandboxed AI
// browser (Aside and friends): that runtime is walled off from $HOME, cannot
// run the CLI, and cannot attach to a stdio pipe. The ONLY channel it has into
// this machine is a real Chrome tab that can reach 127.0.0.1. So we expose the
// same memory tools over a tiny loopback HTTP server the browser can fetch.
//
// This is additive and reuses the MCP dispatcher (callMCPTool), so tool names,
// argument shapes, and payloads match the stdio ones. The generic /call route is
// allowlisted to the non-destructive tools (it never reaches delete_memory); see
// httpCallAllowed.
//
// ZERO-EGRESS / SAFETY
// --------------------
//   - Binds 127.0.0.1 only (loopback is not egress). Never 0.0.0.0.
//   - Bearer token required on every data endpoint; constant-time compared.
//   - Host header allowlist (127.0.0.1 / localhost / [::1] only) defeats DNS
//     rebinding, so a random web page cannot proxy a browser into the vault.
//   - No CORS allow-origin header is ever sent, so a cross-origin page cannot
//     read a response body even if it guesses the token.
//   - The generic /call route is allowlisted to non-destructive tools, so an
//     injected page cannot drive an irreversible delete_memory through the
//     token-holding agent (whose whole job is reading untrusted web pages).
//   - Requests are serialized behind one mutex to mirror the single-flight
//     semantics of the stdio server and avoid index-open contention.

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultHTTPPort = 7777

// httpCallAllowed is the allowlist of MCP tools reachable through POST /call, the
// generic escape hatch. It deliberately EXCLUDES delete_memory: the token holder
// is a sandboxed AI browser that reads untrusted web pages, so a prompt-injected
// page must not be able to drive an irreversible os.Remove of a memory through
// the authorized agent. This is an explicit allowlist (not a delete_memory
// denylist) so a destructive tool added to callMCPTool later is not auto-exposed.
// The named convenience routes are unaffected — none of them reaches
// delete_memory. A future read-only capability token could tighten this further
// (e.g. drop write_memory/read_memory); for now write_memory stays because the
// named /write route already exposes it.
var httpCallAllowed = map[string]bool{
	"brief":          true,
	"context_memory": true,
	"digest":         true,
	"get_entity":     true,
	"list_entities":  true,
	"list_memory":    true,
	"meeting_prep":   true,
	"read_memory":    true,
	"search_memory":  true,
	"think":          true,
	"write_memory":   true,
}

// httpConfig is the on-disk shape of ~/.config/mora/http.json. It carries only
// the bearer token; the port is a runtime flag/env, not persisted state.
type httpConfig struct {
	Token string `json:"token"`
}

// cmdServe dispatches `mora serve <subcommand>`. Today only `http` exists; the
// verb is deliberately generic so future transports (e.g. an SSE MCP endpoint)
// can slot in beside it without a new top-level command.
func cmdServe(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] != "http" {
		return errors.New("usage: mora serve http [install|uninstall|status] [--port 7777] [--print-token]")
	}
	rest := args[1:]
	if len(rest) > 0 {
		switch rest[0] {
		case "install", "uninstall", "status":
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			return serveHTTPService(cfg, rest[0], stdout)
		}
	}
	return serveLoopbackHTTP(ctx, rest, stdout)
}

// serveLoopbackHTTP starts the loopback HTTP server and blocks until ctx is
// cancelled (Ctrl-C) or the listener fails.
func serveLoopbackHTTP(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("serve http", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	port := fs.Int("port", envPortOr(defaultHTTPPort), "loopback port to listen on (or set MORA_PORT)")
	printToken := fs.Bool("print-token", false, "print the bearer token and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	token, err := loadOrCreateHTTPToken(cfg)
	if err != nil {
		return err
	}
	if *printToken {
		fmt.Fprintln(stdout, token)
		return nil
	}

	// Guard the flag path (unlike envPortOr, flag.Int does no range check). Port 0
	// is the trap: net.Listen("127.0.0.1:0") binds a RANDOM ephemeral port and
	// succeeds, but the hostGuard allowlist is built from *port, so every request
	// would then 403 against the real port with no error.
	if *port <= 0 || *port > 65535 {
		return fmt.Errorf("invalid --port %d: must be between 1 and 65535", *port)
	}

	srv := &httpServer{token: token, port: *port}
	handler := srv.hostGuard(srv.auth(srv.routes()))

	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(*port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("loopback listen on %s: %w (is another `mora serve http` already running?)", addr, err)
	}

	httpSrv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	// Graceful shutdown: when the caller cancels ctx (Ctrl-C), drain and stop.
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
	}()

	fmt.Fprintf(stdout, "mora serve http listening on http://%s/  (loopback only)\n", addr)
	fmt.Fprintf(stdout, "  token: %s\n", token)
	fmt.Fprintf(stdout, "  open  http://%s/  in the browser you want to connect (token is embedded there as window.__MORA_TOKEN)\n", addr)

	if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// envPortOr reads MORA_PORT, falling back to def when unset or unparseable.
func envPortOr(def int) int {
	if v := os.Getenv("MORA_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n < 65536 {
			return n
		}
	}
	return def
}

// loadOrCreateHTTPToken reads the persisted bearer token, minting and writing a
// fresh 32-byte hex token (0600) on first run. The token is stable across
// restarts so a connected browser tab keeps working.
func loadOrCreateHTTPToken(cfg Config) (string, error) {
	path := filepath.Join(cfg.ConfigDir, "http.json")
	if b, err := os.ReadFile(path); err == nil {
		var hc httpConfig
		if json.Unmarshal(b, &hc) == nil && hc.Token != "" {
			return hc.Token, nil
		}
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw)
	if err := os.MkdirAll(cfg.ConfigDir, 0o700); err != nil {
		return "", err
	}
	b, _ := json.MarshalIndent(httpConfig{Token: token}, "", "  ")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return "", err
	}
	return token, nil
}

// httpServer holds the request-serialization mutex and the auth token.
type httpServer struct {
	token string
	port  int
	mu    sync.Mutex // serialize tool calls → stdio-equivalent single-flight
}

// hostGuard rejects any request whose Host header is not a loopback name on our
// port. This is the DNS-rebinding defense: a malicious page that rebinds its
// hostname to 127.0.0.1 still sends its own Host header, which we refuse.
func (s *httpServer) hostGuard(next http.Handler) http.Handler {
	allowed := map[string]bool{
		net.JoinHostPort("127.0.0.1", strconv.Itoa(s.port)): true,
		net.JoinHostPort("localhost", strconv.Itoa(s.port)): true,
		net.JoinHostPort("::1", strconv.Itoa(s.port)):       true,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowed[r.Host] {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// auth enforces the bearer token on every route except the liveness probe and
// the landing page (which the browser must be able to load to READ the token).
func (s *httpServer) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/" {
			next.ServeHTTP(w, r)
			return
		}
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// httpRoute is one route's registry entry — the DERIVED source of truth
// routes() ranges over (C3 ▸R2). HTTP routes used to be imperative
// mux.HandleFunc calls with no production registry for the health-surface
// completeness check to enumerate against; httpRoutes() is that registry.
type httpRoute struct {
	Method  string
	Pattern string
	Handler http.HandlerFunc
}

// httpRoutes is the derived list every real route comes from — routes()
// builds the ServeMux from it, and it is the registry
// TestEverySurfaceCarriesHealth enumerates (minus the explicit health-exempt
// allowlist) to prove no route is silently absent from the health check.
func (s *httpServer) httpRoutes() []httpRoute {
	return []httpRoute{
		// GET /healthz is a LIVENESS probe, not a rendered/typed-payload surface
		// — it reports Health.State (never a static {"ok":true} again) but is
		// exempt from the rendered-banner completeness set (C3).
		{"GET", "/healthz", s.handleHealthz},
		{"GET", "/{$}", s.landing},
		// Generic escape hatch: POST {"name":"search_memory","arguments":{...}}.
		{"POST", "/call", s.handleCall},
		// Convenience routes (the shapes the mora-memory Aside skill documents).
		{"POST", "/think", s.handleThink},
		{"POST", "/search", s.handleSearch},
		{"POST", "/write", s.handleWrite},
		{"POST", "/meeting-prep", s.handleMeetingPrep},
		{"GET", "/entity/{name}", s.handleEntity},
		{"GET", "/brief", s.handleBrief},
	}
}

// routes wires httpRoutes() onto a ServeMux.
func (s *httpServer) routes() http.Handler {
	mux := http.NewServeMux()
	for _, rt := range s.httpRoutes() {
		mux.HandleFunc(rt.Method+" "+rt.Pattern, rt.Handler)
	}
	return mux
}

// handleHealthz reports REAL health (C3 ▸R2): a monitor's green light over a
// dead corpus is the purest form of the bug this gate exists to close. Still a
// LIVENESS probe — always HTTP 200 (the SERVER is up even when the DATA is
// not) — so a caller reads `state`/`ok`, never an HTTP error code, to learn
// the vault is unhealthy.
func (s *httpServer) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	cfg, err := loadConfig()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "service": "mora", "version": BuildVersion, "state": healthUnhealthy, "error": err.Error()})
		return
	}
	h := healthOf(cfg, time.Now())
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      h.State == healthHealthy,
		"service": "mora",
		"version": BuildVersion,
		"state":   h.State,
	})
}

func (s *httpServer) handleCall(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if !httpCallAllowed[body.Name] {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "tool not permitted over loopback HTTP", "tool": body.Name})
		return
	}
	s.dispatch(w, r, body.Name, body.Arguments)
}

func (s *httpServer) handleThink(w http.ResponseWriter, r *http.Request) {
	args := bodyArgs(r)
	s.dispatch(w, r, "think", map[string]any{
		"query": firstStr(args, "q", "query"),
		"scope": str(args, "scope"),
		"limit": args["limit"],
	})
}

func (s *httpServer) handleSearch(w http.ResponseWriter, r *http.Request) {
	args := bodyArgs(r)
	s.dispatch(w, r, "search_memory", map[string]any{
		"query": firstStr(args, "q", "query"),
		"scope": str(args, "scope"),
		"limit": args["limit"],
	})
}

func (s *httpServer) handleWrite(w http.ResponseWriter, r *http.Request) {
	args := bodyArgs(r)
	s.dispatch(w, r, "write_memory", map[string]any{
		"title":  str(args, "title"),
		"text":   str(args, "text"),
		"type":   str(args, "type"),
		"scope":  str(args, "scope"),
		"source": str(args, "source"),
	})
}

func (s *httpServer) handleMeetingPrep(w http.ResponseWriter, r *http.Request) {
	args := bodyArgs(r)
	s.dispatch(w, r, "meeting_prep", map[string]any{
		"event_id":   str(args, "event_id"),
		"at":         str(args, "at"),
		"name":       str(args, "name"),
		"limit":      args["limit"],
		"max_tokens": args["max_tokens"],
	})
}

func (s *httpServer) handleEntity(w http.ResponseWriter, r *http.Request) {
	s.dispatch(w, r, "get_entity", map[string]any{"name": r.PathValue("name")})
}

func (s *httpServer) handleBrief(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	args := map[string]any{
		"entity":   q.Get("entity"),
		"scope":    q.Get("scope"),
		"envelope": q.Get("envelope") == "true" || q.Get("envelope") == "1",
	}
	if n, ok := queryInt(q, "max_tokens"); ok {
		args["max_tokens"] = n
	}
	if n, ok := queryInt(q, "since_days"); ok {
		args["since_days"] = n
	}
	s.dispatch(w, r, "brief", args)
}

// dispatch runs one MCP tool under the serialization lock and writes its native
// structured value as the JSON body. Tool errors become HTTP 400 with a JSON
// {"error": ...} envelope so the browser client can react without parsing prose.
func (s *httpServer) dispatch(w http.ResponseWriter, r *http.Request, name string, args map[string]any) {
	pruneNil(args)
	s.mu.Lock()
	v, err := callMCPTool(r.Context(), name, args)
	s.mu.Unlock()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "tool": name})
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// landing serves a minimal page whose only job is to expose the bearer token to
// SAME-ORIGIN page script as window.__MORA_TOKEN. Because the Host guard and the
// same-origin policy both apply, only a tab actually loaded from this loopback
// origin can read it — a cross-origin page cannot.
func (s *httpServer) landing(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>mora loopback</title>
<script>window.__MORA_TOKEN=%q;</script>
<body style="font:14px/1.5 system-ui;max-width:40rem;margin:3rem auto;color:#222">
<h1>mora is serving locally</h1>
<p>This origin exposes your vault's memory tools over loopback HTTP for a connected
AI browser. The bearer token for this session is embedded on this page as
<code>window.__MORA_TOKEN</code> and readable only by same-origin script.</p>
<p>Endpoints: <code>GET /healthz</code>, <code>GET /brief</code>,
<code>POST /think</code>, <code>POST /search</code>, <code>GET /entity/{name}</code>,
<code>POST /meeting-prep</code>, <code>POST /write</code>, <code>POST /call</code>.</p>
</body>`, s.token)
}

// ---- small helpers -------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// decodeBody reads a JSON request body into dst, tolerating an empty body.
func decodeBody(r *http.Request, dst any) error {
	defer r.Body.Close()
	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil
	}
	return json.Unmarshal(b, dst)
}

// bodyArgs decodes a JSON body into a generic arg map (numbers arrive as
// float64, exactly like the MCP path expects), never nil.
func bodyArgs(r *http.Request) map[string]any {
	m := map[string]any{}
	_ = decodeBody(r, &m)
	if m == nil {
		m = map[string]any{}
	}
	return m
}

func str(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// firstStr returns the first non-empty string among the given keys.
func firstStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func queryInt(q map[string][]string, key string) (float64, bool) {
	vs, ok := q[key]
	if !ok || len(vs) == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(vs[0])
	if err != nil {
		return 0, false
	}
	return float64(n), true
}

// pruneNil drops nil/empty-string entries so a convenience route that forwarded
// an absent field doesn't override a tool's own default.
func pruneNil(m map[string]any) {
	for k, v := range m {
		if v == nil {
			delete(m, k)
			continue
		}
		if s, ok := v.(string); ok && s == "" {
			delete(m, k)
		}
	}
}
