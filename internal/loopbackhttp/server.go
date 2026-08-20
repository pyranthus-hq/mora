// Package loopbackhttp owns the authenticated loopback HTTP transport for Mora tool callbacks.
package loopbackhttp

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
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

type DispatchFunc func(context.Context, string, map[string]any) (any, error)
type Health struct {
	OK    bool
	State string
	Err   error
}
type Options struct {
	Token     string
	Port      int
	Version   string
	Dispatch  DispatchFunc
	Health    func() Health
	AllowCall func(string) bool
}
type Server struct {
	token      string
	port       int
	version    string
	dispatchFn DispatchFunc
	healthFn   func() Health
	allowCall  func(string) bool
	mu         sync.Mutex
}

func New(o Options) *Server {
	return &Server{token: o.Token, port: o.Port, version: o.Version, dispatchFn: o.Dispatch, healthFn: o.Health, allowCall: o.AllowCall}
}

type Route struct{ Method, Pattern string }
type route struct {
	Method, Pattern string
	Handler         http.HandlerFunc
}
type httpConfig struct {
	Token string `json:"token"`
}

func LoadOrCreateToken(configDir string) (string, error) {
	path := filepath.Join(configDir, "http.json")
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
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return "", err
	}
	b, _ := json.MarshalIndent(httpConfig{Token: token}, "", "  ")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return "", err
	}
	return token, nil
}
func (s *Server) hostGuard(next http.Handler) http.Handler {
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
func (s *Server) auth(next http.Handler) http.Handler {
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
func (s *Server) routeDefs() []route {
	return []route{
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
func (s *Server) router() http.Handler {
	mux := http.NewServeMux()
	for _, rt := range s.routeDefs() {
		mux.HandleFunc(rt.Method+" "+rt.Pattern, rt.Handler)
	}
	return mux
}
func (s *Server) handleCall(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if s.allowCall == nil || !s.allowCall(body.Name) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "tool not permitted over loopback HTTP", "tool": body.Name})
		return
	}
	s.dispatch(w, r, body.Name, body.Arguments)
}
func (s *Server) handleThink(w http.ResponseWriter, r *http.Request) {
	args := bodyArgs(r)
	s.dispatch(w, r, "think", map[string]any{
		"query": firstStr(args, "q", "query"),
		"scope": str(args, "scope"),
		"limit": args["limit"],
	})
}
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	args := bodyArgs(r)
	s.dispatch(w, r, "search_memory", map[string]any{
		"query": firstStr(args, "q", "query"),
		"scope": str(args, "scope"),
		"limit": args["limit"],
	})
}
func (s *Server) handleWrite(w http.ResponseWriter, r *http.Request) {
	args := bodyArgs(r)
	s.dispatch(w, r, "write_memory", map[string]any{
		"title":  str(args, "title"),
		"text":   str(args, "text"),
		"type":   str(args, "type"),
		"scope":  str(args, "scope"),
		"source": str(args, "source"),
	})
}
func (s *Server) handleMeetingPrep(w http.ResponseWriter, r *http.Request) {
	args := bodyArgs(r)
	s.dispatch(w, r, "meeting_prep", map[string]any{
		"event_id":   str(args, "event_id"),
		"at":         str(args, "at"),
		"name":       str(args, "name"),
		"limit":      args["limit"],
		"max_tokens": args["max_tokens"],
	})
}
func (s *Server) handleEntity(w http.ResponseWriter, r *http.Request) {
	s.dispatch(w, r, "get_entity", map[string]any{"name": r.PathValue("name")})
}
func (s *Server) handleBrief(w http.ResponseWriter, r *http.Request) {
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
func (s *Server) dispatch(w http.ResponseWriter, r *http.Request, name string, args map[string]any) {
	pruneNil(args)
	s.mu.Lock()
	v, err := s.dispatchFn(r.Context(), name, args)
	s.mu.Unlock()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "tool": name})
		return
	}
	writeJSON(w, http.StatusOK, v)
}
func (s *Server) landing(w http.ResponseWriter, _ *http.Request) {
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
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
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

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	h := Health{State: "unhealthy"}
	if s.healthFn != nil {
		h = s.healthFn()
	}
	v := map[string]any{"ok": h.OK, "service": "mora", "version": s.version, "state": h.State}
	if h.Err != nil {
		v["error"] = h.Err.Error()
	}
	writeJSON(w, http.StatusOK, v)
}
func (s *Server) Routes() []Route {
	defs := s.routeDefs()
	out := make([]Route, 0, len(defs))
	for _, r := range defs {
		out = append(out, Route{Method: r.Method, Pattern: r.Pattern})
	}
	return out
}
func (s *Server) Handler() http.Handler { return s.hostGuard(s.auth(s.router())) }
func (s *Server) Serve(ctx context.Context, stdout io.Writer) error {
	if s.port <= 0 || s.port > 65535 {
		return fmt.Errorf("invalid --port %d: must be between 1 and 65535", s.port)
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(s.port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("loopback listen on %s: %w (is another `mora serve http` already running?)", addr, err)
	}
	hs := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hs.Shutdown(c)
	}()
	fmt.Fprintf(stdout, "mora serve http listening on http://%s/  (loopback only)\n", addr)
	fmt.Fprintf(stdout, "  token: %s\n", s.token)
	fmt.Fprintf(stdout, "  open  http://%s/  in the browser you want to connect (token is embedded there as window.__MORA_TOKEN)\n", addr)
	if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
