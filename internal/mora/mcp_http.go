package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	mcppkg "github.com/pyranthus-hq/mora/internal/mcp"
)

// `mora mcp serve-http` (#539 phase 2) serves MCP over Streamable HTTP for
// clients that cannot spawn a local process: cloud agents that reach the
// machine through a tunnel the user runs (Tailscale Funnel, a Cloudflare
// tunnel). The server itself only ever binds 127.0.0.1; exposure is the
// tunnel's job and the user's decision.
//
// Every request must carry `Authorization: Bearer <agent token>`, and the token
// selects exactly one agent profile, which then governs the call end to end
// (agents.go). There is no unprofiled mode: with no active profile the server
// refuses to start. The server answers JSON only (no SSE stream), accepts no
// batches, and refuses any request with an Origin header, because its callers
// are servers, never browsers.

const (
	mcpHTTPDefaultPort   = 7780
	mcpHTTPLatestVersion = "2025-06-18"
)

// mcpHTTPProtocolVersions are the MCP revisions this JSON-only transport can
// honestly answer. An initialize naming one of them gets it echoed back;
// anything else is offered the latest.
var mcpHTTPProtocolVersions = map[string]bool{
	mcppkg.ProtocolVersion: true,
	"2025-03-26":           true,
	"2025-06-18":           true,
}

type mcpHTTPServer struct {
	port         int
	allowedHosts map[string]bool
	loadStore    func() (agentsStore, error)
}

func cmdMCPServeHTTP(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("mcp serve-http", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	port := fs.Int("port", mcpHTTPDefaultPort, "loopback port to listen on")
	allowHost := fs.String("allow-host", "", "comma-separated public hostnames the tunnel forwards (e.g. host.tailnet.ts.net)")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return errors.New("usage: mora mcp serve-http [--port 7780] [--allow-host host.example.ts.net]")
	}
	if *port <= 0 || *port > 65535 {
		return fmt.Errorf("invalid --port %d", *port)
	}
	cfg, err := loadConfigFor(ctx)
	if err != nil {
		return err
	}
	store, err := loadAgentsStore(cfg)
	if err != nil {
		return err
	}
	if store.activeCount() == 0 {
		return errors.New("no active agent profiles: create one with `mora agents add <name> --read-scopes <scope>` before serving MCP over HTTP")
	}
	srv := newMCPHTTPServer(*port, splitList(*allowHost), func() (agentsStore, error) {
		c, err := loadConfigFor(ctx)
		if err != nil {
			return agentsStore{}, err
		}
		return loadAgentsStore(c)
	})
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(*port)))
	if err != nil {
		return err
	}
	httpSrv := &http.Server{
		Handler:           srv.handler(ctx),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    16 << 10,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	fmt.Fprintf(stdout, "mora MCP (streamable HTTP) on http://127.0.0.1:%d/mcp for %d agent profile(s)\n", *port, store.activeCount())
	if len(srv.allowedHosts) > 0 {
		hosts := make([]string, 0, len(srv.allowedHosts))
		for h := range srv.allowedHosts {
			hosts = append(hosts, h)
		}
		fmt.Fprintf(stdout, "accepting forwarded Host: %s\n", strings.Join(hosts, ", "))
	}
	errc := make(chan error, 1)
	go func() { errc <- httpSrv.Serve(ln) }()
	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdown)
		return nil
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func newMCPHTTPServer(port int, extraHosts []string, loadStore func() (agentsStore, error)) *mcpHTTPServer {
	allowed := map[string]bool{}
	for _, h := range []string{"127.0.0.1", "localhost", "[::1]"} {
		allowed[h+":"+strconv.Itoa(port)] = true
	}
	for _, h := range extraHosts {
		h = strings.ToLower(strings.TrimSuffix(h, "."))
		allowed[h] = true
	}
	return &mcpHTTPServer{port: port, allowedHosts: allowed, loadStore: loadStore}
}

func (s *mcpHTTPServer) hostAllowed(host string) bool {
	host = strings.ToLower(host)
	if s.allowedHosts[host] {
		return true
	}
	// A tunnel may forward the public name with an explicit :443.
	if h, p, err := net.SplitHostPort(host); err == nil && p == "443" {
		return s.allowedHosts[h]
	}
	return false
}

func (s *mcpHTTPServer) handler(base context.Context) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		mcpHTTPWriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("/mcp", s.serveMCP)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.hostAllowed(r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if r.Header.Get("Origin") != "" {
			http.Error(w, "browser origins are not accepted", http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, r)
	})
}

func jsonRPCError(id any, code int, msg string) jsonRPCResponse {
	return jsonRPCResponse{JSONRPC: "2.0", ID: id, Error: map[string]any{"code": code, "message": msg}}
}

func (s *mcpHTTPServer) serveMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed: this server answers POST with JSON and offers no SSE stream", http.StatusMethodNotAllowed)
		return
	}
	token := bearerToken(r.Header.Get("Authorization"))
	if token == "" {
		w.Header().Set("WWW-Authenticate", `Bearer realm="mora"`)
		mcpHTTPWriteJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	store, err := s.loadStore()
	if err != nil {
		mcpHTTPWriteJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "agent profiles unavailable"})
		return
	}
	profile, ok := store.matchToken(token)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="mora", error="invalid_token"`)
		mcpHTTPWriteJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, int64(mcpMaxRequestBytes)))
	if err != nil {
		mcpHTTPWriteJSON(w, http.StatusRequestEntityTooLarge, jsonRPCError(nil, -32600, "request too large"))
		return
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		mcpHTTPWriteJSON(w, http.StatusBadRequest, jsonRPCError(nil, -32600, "JSON-RPC batches are not supported"))
		return
	}
	var req jsonRPCRequest
	if err := json.Unmarshal(trimmed, &req); err != nil || req.Method == "" {
		mcpHTTPWriteJSON(w, http.StatusBadRequest, jsonRPCError(nil, -32700, "parse error"))
		return
	}
	if req.ID == nil {
		// Notifications (notifications/initialized and friends) need no answer.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	ctx := withAgentProfile(r.Context(), profile)
	mcpHTTPWriteJSON(w, http.StatusOK, handleAgentMCP(ctx, profile, req))
}

// bearerToken returns the credential in an Authorization header. The scheme is
// matched without regard to case (RFC 9110, section 11.1). A bare agent token
// is accepted too, because some connector forms send the value of their
// "Authorization" field with no scheme; the token is still checked in full.
func bearerToken(header string) string {
	header = strings.TrimSpace(header)
	if scheme, rest, ok := strings.Cut(header, " "); ok && strings.EqualFold(scheme, "Bearer") {
		return strings.TrimSpace(rest)
	}
	if strings.HasPrefix(header, agentTokenPrefix) {
		return header
	}
	return ""
}

// handleAgentMCP is handleMCP for a profile-bound client: the handshake
// describes the profile's write authority, tools/list shows only its tools,
// and tools/call runs through invokeMCPTool, which enforces the profile.
func handleAgentMCP(ctx context.Context, p agentProfile, req jsonRPCRequest) jsonRPCResponse {
	switch req.Method {
	case "ping":
		return jsonRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{}}
	}
	return mcppkg.Dispatch(ctx, req,
		func() any {
			_, err := loadConfigFor(ctx)
			result := mcppkg.InitializeResult(BuildVersion, p.Write, err == nil)
			result["protocolVersion"] = negotiateMCPVersion(req.Params)
			if instr, ok := result["instructions"].(string); ok && err == nil {
				result["instructions"] = agentInstructions(p, instr)
			}
			return result
		},
		func() any { return map[string]any{"tools": mcppkg.RenderTools(agentToolDefs(p))} },
		func(ctx context.Context, name string, args map[string]any) any {
			return invokeMCPTool(ctx, name, args).result()
		},
	)
}

func negotiateMCPVersion(params json.RawMessage) string {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &p)
	if mcpHTTPProtocolVersions[p.ProtocolVersion] {
		return p.ProtocolVersion
	}
	return mcpHTTPLatestVersion
}

// agentInstructions prefixes the standard server instructions with the
// profile's boundary, so the agent knows what it cannot see before it
// concludes the vault lacks something.
func agentInstructions(p agentProfile, base string) string {
	scopes := strings.Join(p.ReadScopes, ", ")
	raw := "Connector evidence (mail, messages, calendar, files) is NOT visible to this connection; only authored memories are."
	if p.RawSources {
		raw = "Connector evidence is visible to this connection."
	}
	return fmt.Sprintf("This connection is the %q agent profile. It can read scopes: %s. %s Tools outside this profile do not exist for it. An empty result means nothing matched inside this boundary, not that the owner's memory is empty.\n\n%s",
		p.Name, scopes, raw, base)
}

func mcpHTTPWriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
