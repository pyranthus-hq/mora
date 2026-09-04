package companion

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubReader answers the three routes from fixtures. Every field is settable so
// a test can make the kernel fail one route without touching the others.
type stubReader struct {
	today     TodayProjection
	context   ContextBundle
	health    HealthProjection
	todayErr  error
	ctxErr    error
	healthErr error
	lastReq   ContextRequest
	calls     int
	// entered is signalled once per kernel call, and hold (when non-nil) is
	// waited on before the call returns. Together they let a test pin one
	// request inside the kernel while it drives a second.
	entered chan struct{}
	hold    chan struct{}
	// honorContext makes a call return the context's error instead of an
	// answer, which is what a real kernel read does when the deadline fires.
	honorContext bool
}

// gate is the shared body of every stubbed kernel call.
func (s *stubReader) gate(ctx context.Context) error {
	s.calls++
	if s.entered != nil {
		s.entered <- struct{}{}
	}
	if s.honorContext {
		<-ctx.Done()
		return ctx.Err()
	}
	if s.hold != nil {
		<-s.hold
	}
	return ctx.Err()
}

func newStubReader() *stubReader {
	return &stubReader{today: *TodayFixture(), context: *ContextFixture(), health: *HealthFixture()}
}

func (s *stubReader) Today(ctx context.Context) (TodayProjection, error) {
	if err := s.gate(ctx); err != nil {
		return TodayProjection{}, err
	}
	return s.today, s.todayErr
}
func (s *stubReader) Health(ctx context.Context) (HealthProjection, error) {
	if err := s.gate(ctx); err != nil {
		return HealthProjection{}, err
	}
	return s.health, s.healthErr
}
func (s *stubReader) Context(ctx context.Context, req ContextRequest) (ContextBundle, error) {
	s.lastReq = req
	if err := s.gate(ctx); err != nil {
		return ContextBundle{}, err
	}
	return s.context, s.ctxErr
}

// testServer wires a real registry to a stub kernel and returns a live device
// token beside them. The registry is real on purpose: the credential half of
// this listener is exactly the thing worth testing against production code.
func testServer(t *testing.T) (*Server, *stubReader, *Registry, string, *bytes.Buffer) {
	t.Helper()
	reg, _, _, _ := testRegistry(t)
	token, _ := pairAndConfirm(t, reg, "phone")
	reader := newStubReader()
	log := &bytes.Buffer{}
	srv, err := NewServer(ServerOptions{
		Addr:    "127.0.0.1:7778",
		Devices: reg,
		Reader:  reader,
		Now:     func() time.Time { return time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC) },
		Log:     log,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv, reader, reg, token, log
}

func request(method, path, token string, body io.Reader) *http.Request {
	r := httptest.NewRequest(method, path, body)
	r.Host = "127.0.0.1:7778"
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

func contextBody(t *testing.T, mode ContextMode, query string) io.Reader {
	t.Helper()
	req := NewContextRequest()
	req.Mode = mode
	req.Query = query
	body, err := Marshal(&req)
	if err != nil {
		t.Fatalf("marshal context request: %v", err)
	}
	return bytes.NewReader(body)
}

// ---------------------------------------------------------------------------
// The allowlist
// ---------------------------------------------------------------------------

// TestServerServesExactlyThreeRoutes is the route-table gate. It walks the
// declared table AND drives the mux, so a route added to one without the other
// is caught either way.
func TestServerServesExactlyThreeRoutes(t *testing.T) {
	srv, _, _, token, _ := testServer(t)

	want := []Route{
		{Method: http.MethodGet, Pattern: "/v1/companion/today"},
		{Method: http.MethodPost, Pattern: "/v1/companion/context"},
		{Method: http.MethodGet, Pattern: "/v1/companion/health"},
	}
	got := srv.Routes()
	if len(got) != len(want) {
		t.Fatalf("the listener declares %d routes, want exactly %d: %+v", len(got), len(want), got)
	}
	for i, route := range want {
		if got[i] != route {
			t.Fatalf("route %d = %+v, want %+v", i, got[i], route)
		}
	}

	handler := srv.Handler()
	for _, route := range want {
		var body io.Reader
		if route.Method == http.MethodPost {
			body = contextBody(t, ModeThink, "what did Sam decide")
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, request(route.Method, route.Pattern, token, body))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s = %d, want 200\n%s", route.Method, route.Pattern, rec.Code, rec.Body.String())
		}
	}
}

// TestServerRefusesEveryRouteOutsideTheAllowlist drives the exact surfaces a
// device must never reach, WITH a live credential so the refusal is the route
// table's doing and not the authenticator's.
//
// The generic loopback API's whole surface is in this list. That is the point:
// the two servers are separate so that adding a tool to `mora serve http` can
// never widen what a phone can do.
func TestServerRefusesEveryRouteOutsideTheAllowlist(t *testing.T) {
	srv, reader, _, token, _ := testServer(t)
	handler := srv.Handler()

	declared := map[string]bool{}
	for _, route := range srv.Routes() {
		declared[route.Method+" "+route.Pattern] = true
	}

	for _, tc := range []struct{ method, path string }{
		// The generic loopback API, route for route.
		{http.MethodPost, "/call"},
		{http.MethodPost, "/think"},
		{http.MethodPost, "/search"},
		{http.MethodPost, "/write"},
		{http.MethodPost, "/meeting-prep"},
		{http.MethodGet, "/brief"},
		{http.MethodGet, "/healthz"},
		{http.MethodGet, "/entity/Sam"},
		{http.MethodGet, "/"},
		// Mutation and administration, in the companion namespace.
		{http.MethodPost, "/v1/companion/captures"},
		{http.MethodDelete, "/v1/companion/memories/mem_1"},
		{http.MethodGet, "/v1/companion/memories/mem_1"},
		{http.MethodPost, "/v1/companion/sync"},
		{http.MethodGet, "/v1/companion/connectors"},
		{http.MethodPost, "/v1/companion/config"},
		{http.MethodGet, "/v1/companion/devices"},
		{http.MethodPost, "/v1/companion/call"},
		{http.MethodGet, "/v1/companion/operations"},
		// Traversal back out of the namespace.
		{http.MethodGet, "/v1/companion/today/../../call"},
		{http.MethodGet, "/v1/companion"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			// Mounted-ness first, then behavior. A forbidden route added to
			// the table is caught even when a probe body happens to make its
			// handler answer 4xx for an unrelated reason — the refusal must be
			// "this route does not exist", not "your body was malformed".
			if declared[tc.method+" "+tc.path] {
				t.Fatalf("%s %s is on the declared allowlist; it must not be", tc.method, tc.path)
			}
			before := reader.calls
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, request(tc.method, tc.path, token, strings.NewReader("{}")))
			if rec.Code == http.StatusOK {
				t.Fatalf("%s %s answered 200; it is not on the allowlist\n%s", tc.method, tc.path, rec.Body.String())
			}
			if reader.calls != before {
				t.Fatalf("%s %s reached the kernel", tc.method, tc.path)
			}
		})
	}
}

// TestServerRefusesEveryMethodOutsideTheAllowlist is the method half of the
// allowlist, driven against the REAL mux for EVERY method the standard library
// knows.
//
// HEAD is the reason this test is shaped this way. Go's ServeMux answers a
// "GET /x" pattern for HEAD as well, so registering the method in the pattern
// served a method Routes() never listed: HEAD reached the kernel and returned
// its headers while the declared allowlist said GET only. Enumerating the
// method set rather than spot-checking a few verbs is what catches the next one
// of those.
func TestServerRefusesEveryMethodOutsideTheAllowlist(t *testing.T) {
	srv, reader, _, token, _ := testServer(t)
	handler := srv.Handler()

	allowed := map[string]string{}
	for _, route := range srv.Routes() {
		allowed[route.Pattern] = route.Method
	}
	methods := []string{
		http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions, http.MethodConnect,
		http.MethodTrace,
	}

	for _, pattern := range []string{RouteToday, RouteContext, RouteHealth} {
		for _, method := range methods {
			if method == allowed[pattern] || method == http.MethodConnect {
				// CONNECT is not a request httptest can shape meaningfully.
				continue
			}
			t.Run(method+" "+pattern, func(t *testing.T) {
				before := reader.calls
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, request(method, pattern, token, strings.NewReader("{}")))
				if rec.Code != http.StatusMethodNotAllowed {
					t.Fatalf("%s %s = %d, want 405\n%s", method, pattern, rec.Code, rec.Body.String())
				}
				if got := rec.Header().Get("Allow"); got != allowed[pattern] {
					t.Fatalf("%s %s Allow = %q, want %q", method, pattern, got, allowed[pattern])
				}
				if reader.calls != before {
					t.Fatalf("%s %s reached the kernel", method, pattern)
				}
				if rec.Body.Len() != 0 && strings.Contains(rec.Body.String(), "schema") {
					t.Fatalf("%s %s answered with a projection:\n%s", method, pattern, rec.Body.String())
				}
			})
		}
	}
}

// TestServerMountsNothingOutsideTheDeclaredRoutes probes the REAL mux for paths
// no routeDef names.
//
// The previous version of the allowlist test enumerated its expectations from
// routeDefs, which is the table under test: a stray `mux.HandleFunc` added
// beside the loop would have been invisible to it. This drives a table of probe
// paths the mux must not know, with a live credential and the permissive method,
// so a registration outside routeDefs shows up as a non-404.
func TestServerMountsNothingOutsideTheDeclaredRoutes(t *testing.T) {
	srv, reader, _, token, _ := testServer(t)
	handler := srv.Handler()

	declared := map[string]bool{}
	for _, route := range srv.Routes() {
		declared[route.Pattern] = true
	}

	// Every path a registration would plausibly use, plus the prefixes a
	// subtree pattern ("/v1/", "/") would capture.
	probes := []string{
		"/", "/v1", "/v1/", "/v1/companion", "/v1/companion/",
		"/call", "/healthz", "/brief", "/search", "/think", "/write", "/entity/Sam",
		"/v1/companion/captures", "/v1/companion/devices", "/v1/companion/operations",
		"/v1/companion/memories", "/v1/companion/sync", "/v1/companion/config",
		"/v1/companion/today/extra", "/v1/companion/health/extra", "/v1/companion/context/extra",
		"/debug/pprof/", "/metrics", "/index.html", "/favicon.ico",
	}
	for _, path := range probes {
		if declared[path] {
			t.Fatalf("probe %q is a declared route; the probe table is stale", path)
		}
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			t.Run(method+" "+path, func(t *testing.T) {
				before := reader.calls
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, request(method, path, token, strings.NewReader("{}")))
				// 404 is the only acceptable answer: 405 would mean the mux
				// knows the path, and anything 2xx would mean it serves it.
				if rec.Code != http.StatusNotFound {
					t.Fatalf("%s %s = %d, want 404 — something is mounted outside routeDefs\n%s",
						method, path, rec.Code, rec.Body.String())
				}
				if reader.calls != before {
					t.Fatalf("%s %s reached the kernel", method, path)
				}
			})
		}
	}
}

// TestServerHeadDoesNotReachTheKernel is the named regression for the defect
// itself, kept separate from the enumeration so it cannot be diluted by a
// future edit to the method table.
func TestServerHeadDoesNotReachTheKernel(t *testing.T) {
	srv, reader, _, token, _ := testServer(t)
	handler := srv.Handler()

	for _, pattern := range []string{RouteToday, RouteHealth} {
		t.Run(pattern, func(t *testing.T) {
			before := reader.calls
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, request(http.MethodHead, pattern, token, nil))
			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("HEAD %s = %d, want 405", pattern, rec.Code)
			}
			if reader.calls != before {
				t.Fatalf("HEAD %s executed the kernel", pattern)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Credentials
// ---------------------------------------------------------------------------

// TestServerNormalizesEveryCredentialFailureToOneOpaque401 is the
// discrimination gate. An unknown device, a revoked device, a malformed token
// and no token at all must be indistinguishable in the status, the body and the
// headers — otherwise a caller holding a stolen token can classify it by
// probing, which is the difference between "this token is dead" and "this token
// belongs to a device that exists".
func TestServerNormalizesEveryCredentialFailureToOneOpaque401(t *testing.T) {
	srv, reader, reg, _, _ := testServer(t)
	revoked, revokedDev := pairAndConfirm(t, reg, "stolen")
	if _, _, err := reg.Revoke(revokedDev.DeviceID); err != nil {
		t.Fatal(err)
	}
	handler := srv.Handler()

	type answer struct {
		code    int
		body    string
		headers string
	}
	answers := map[string]answer{}
	for _, tc := range []struct{ name, header string }{
		{"no header", ""},
		{"empty bearer", "Bearer "},
		{"malformed token", "Bearer not-a-token"},
		{"no scheme", "deadbeef"},
		{"unknown device", "Bearer NEVERISSUEDTOKENVALUE"},
		{"revoked device", "Bearer " + revoked},
		{"wrong scheme", "Basic aGk6aGk="},
	} {
		before := reader.calls
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, RouteToday, nil)
		req.Host = "127.0.0.1:7778"
		if tc.header != "" {
			req.Header.Set("Authorization", tc.header)
		}
		handler.ServeHTTP(rec, req)
		if reader.calls != before {
			t.Fatalf("%s reached the kernel", tc.name)
		}
		answers[tc.name] = answer{code: rec.Code, body: rec.Body.String(), headers: fmt.Sprint(rec.Header())}
	}

	var first string
	for name, got := range answers {
		if got.code != http.StatusUnauthorized {
			t.Fatalf("%s = %d, want 401", name, got.code)
		}
		if strings.Contains(got.headers, "Www-Authenticate") {
			t.Fatalf("%s carries a WWW-Authenticate challenge, which discriminates", name)
		}
		if first == "" {
			first = name
			continue
		}
		if got != answers[first] {
			t.Fatalf("%s answers %+v but %s answers %+v; the two are distinguishable", name, got, first, answers[first])
		}
	}
}

// TestServerRefusesARevokedDeviceImmediately proves revocation is live rather
// than cached: a token that worked a moment ago stops working on the next
// request, with no restart.
func TestServerRefusesARevokedDeviceImmediately(t *testing.T) {
	srv, _, reg, token, _ := testServer(t)
	handler := srv.Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, request(http.MethodGet, RouteToday, token, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("the live device got %d, want 200", rec.Code)
	}

	devices, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := reg.Revoke(devices[0].DeviceID); err != nil {
		t.Fatal(err)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, request(http.MethodGet, RouteToday, token, nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("the revoked device got %d, want 401", rec.Code)
	}
}

// TestServerHandlersAuthorizeWithoutTheMiddleware is the defense-in-depth gate.
// The handlers are reached DIRECTLY, with no guard chain around them, which is
// what a route mounted on the bare mux by a future change would look like.
func TestServerHandlersAuthorizeWithoutTheMiddleware(t *testing.T) {
	srv, reader, _, token, _ := testServer(t)

	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		method  string
		path    string
		body    func() io.Reader
	}{
		{"today", srv.handleToday, http.MethodGet, RouteToday, func() io.Reader { return nil }},
		{"health", srv.handleHealth, http.MethodGet, RouteHealth, func() io.Reader { return nil }},
		{"context", srv.handleContext, http.MethodPost, RouteContext, func() io.Reader { return contextBody(t, ModeThink, "q") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := reader.calls
			rec := httptest.NewRecorder()
			tc.handler(rec, request(tc.method, tc.path, "", tc.body()))
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("the bare handler answered %d without a credential, want 401", rec.Code)
			}
			if reader.calls != before {
				t.Fatalf("the bare handler reached the kernel without a credential")
			}

			rec = httptest.NewRecorder()
			tc.handler(rec, request(tc.method, tc.path, token, tc.body()))
			if rec.Code != http.StatusOK {
				t.Fatalf("the bare handler answered %d with a live credential, want 200\n%s", rec.Code, rec.Body.String())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Binding
// ---------------------------------------------------------------------------

// TestNewServerBindsLoopbackOnly refuses every address that is not the literal
// loopback host, including the ones that USUALLY resolve to it.
func TestNewServerBindsLoopbackOnly(t *testing.T) {
	reg, _, _, _ := testRegistry(t)
	for _, addr := range []string{
		"0.0.0.0:7778",
		"localhost:7778",
		"[::1]:7778",
		"::1:7778",
		"192.168.1.10:7778",
		"100.64.0.1:7778",
		"example.test:7778",
		":7778",
		"127.0.0.1",
		"127.0.0.1:",
		"",
	} {
		t.Run(addr, func(t *testing.T) {
			_, err := NewServer(ServerOptions{Addr: addr, Devices: reg, Reader: newStubReader()})
			if !errors.Is(err, ErrNotLoopback) {
				t.Fatalf("NewServer(%q) = %v, want ErrNotLoopback", addr, err)
			}
		})
	}
	if _, err := NewServer(ServerOptions{Addr: "127.0.0.1:0", Devices: reg, Reader: newStubReader()}); err != nil {
		t.Fatalf("NewServer refused the loopback address: %v", err)
	}
}

// TestServerRefusesANonLoopbackHostHeader is the DNS-rebinding gate. A name that
// resolves to 127.0.0.1 still arrives with that name in the Host header.
func TestServerRefusesANonLoopbackHostHeader(t *testing.T) {
	srv, reader, _, token, _ := testServer(t)
	handler := srv.Handler()

	for _, host := range []string{"localhost:7778", "attacker.test:7778", "mora.local", "[::1]:7778", "0.0.0.0:7778"} {
		t.Run(host, func(t *testing.T) {
			before := reader.calls
			rec := httptest.NewRecorder()
			req := request(http.MethodGet, RouteToday, token, nil)
			req.Host = host
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("Host %q got %d, want 403", host, rec.Code)
			}
			if reader.calls != before {
				t.Fatalf("Host %q reached the kernel", host)
			}
		})
	}
}

// TestServeRefusesANonLoopbackAddress covers the second check, the one inside
// Serve, which exists so a Server assembled by some future constructor cannot
// skip the address rule.
func TestServeRefusesANonLoopbackAddress(t *testing.T) {
	reg, _, _, _ := testRegistry(t)
	srv := &Server{addr: "0.0.0.0:7778", devices: reg, reader: newStubReader(), now: time.Now, log: io.Discard, seen: map[string]time.Time{}}
	if err := srv.Serve(context.Background()); !errors.Is(err, ErrNotLoopback) {
		t.Fatalf("Serve = %v, want ErrNotLoopback", err)
	}
}

// ---------------------------------------------------------------------------
// Bounds
// ---------------------------------------------------------------------------

func TestServerCapsHeadersAndBodies(t *testing.T) {
	srv, reader, _, token, _ := testServer(t)
	handler := srv.Handler()

	t.Run("host header", func(t *testing.T) {
		// net/http promotes Host out of r.Header, so a bound computed from
		// r.Header alone cannot see it. This is the shape that proves the
		// handler-level bound and the listener's MaxHeaderBytes agree.
		before := reader.calls
		rec := httptest.NewRecorder()
		req := request(http.MethodGet, RouteToday, token, nil)
		req.Host = LoopbackHost + ":" + strings.Repeat("7", MaxHeaderBytes)
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusRequestHeaderFieldsTooLarge {
			t.Fatalf("an oversize Host header got %d, want 431", rec.Code)
		}
		if reader.calls != before {
			t.Fatalf("an oversize Host header reached the kernel")
		}
	})

	t.Run("headers", func(t *testing.T) {
		before := reader.calls
		rec := httptest.NewRecorder()
		req := request(http.MethodGet, RouteToday, token, nil)
		req.Header.Set("X-Padding", strings.Repeat("a", MaxHeaderBytes+1))
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusRequestHeaderFieldsTooLarge {
			t.Fatalf("an oversize header set got %d, want 431", rec.Code)
		}
		if reader.calls != before {
			t.Fatalf("an oversize header set reached the kernel")
		}
	})

	t.Run("body", func(t *testing.T) {
		before := reader.calls
		req := NewContextRequest()
		req.Mode = ModeSearch
		req.Query = strings.Repeat("q", MaxQueryBytes)
		body, err := Marshal(&req)
		if err != nil {
			t.Fatal(err)
		}
		// Pad past the request bound with whitespace, which keeps the JSON
		// itself legal so the refusal is the SIZE rule and not the decoder's.
		padded := append(body, bytes.Repeat([]byte(" "), MaxRequestBytes)...)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, request(http.MethodPost, RouteContext, token, bytes.NewReader(padded)))
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("an oversize body got %d, want 413", rec.Code)
		}
		if reader.calls != before {
			t.Fatalf("an oversize body reached the kernel")
		}
	})
}

// ---------------------------------------------------------------------------
// Payloads
// ---------------------------------------------------------------------------

// TestServerDecodesContextStrictly pins the strict-inbound half of N02: unknown
// fields, unknown enum values and malformed documents are refused, and the
// refusal names the code and the field but never the value.
func TestServerDecodesContextStrictly(t *testing.T) {
	srv, reader, _, token, _ := testServer(t)
	handler := srv.Handler()

	secret := "correct-horse-battery-staple"
	for _, tc := range []struct{ name, body string }{
		{"unknown field", `{"schema":"mora.companion.context.request","schema_version":1,"mode":"think","query":"` + secret + `","source":"gmail"}`},
		{"unknown enum", `{"schema":"mora.companion.context.request","schema_version":1,"mode":"summarize","query":"` + secret + `"}`},
		{"wrong schema name", `{"schema":"mora.companion.capture","schema_version":1,"mode":"think","query":"` + secret + `"}`},
		{"missing query", `{"schema":"mora.companion.context.request","schema_version":1,"mode":"think","query":""}`},
		{"trailing data", `{"schema":"mora.companion.context.request","schema_version":1,"mode":"think","query":"` + secret + `"}}`},
		{"not json", `nonsense`},
		{"empty body", ``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := reader.calls
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, request(http.MethodPost, RouteContext, token, strings.NewReader(tc.body)))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s got %d, want 400\n%s", tc.name, rec.Code, rec.Body.String())
			}
			if reader.calls != before {
				t.Fatalf("%s reached the kernel", tc.name)
			}
			if strings.Contains(rec.Body.String(), secret) {
				t.Fatalf("the rejection echoed the query back:\n%s", rec.Body.String())
			}
		})
	}
}

// TestServerRefusesAContextRequestWithNoEnvelope is the strict-inbound
// regression.
//
// The handler used to decode into NewContextRequest(), whose constructor
// pre-fills schema and schema_version — so a body that omitted the envelope
// entirely inherited the right answer and was accepted as a v1 request. The
// envelope is the pinning identity of a payload; a decoder that supplies it on
// the sender's behalf is not validating a version, it is assuming one.
func TestServerRefusesAContextRequestWithNoEnvelope(t *testing.T) {
	srv, reader, _, token, _ := testServer(t)
	handler := srv.Handler()

	for _, tc := range []struct{ name, body, wantField string }{
		{"no envelope at all", `{"mode":"think","query":"x"}`, "schema"},
		{"no schema", `{"schema_version":1,"mode":"think","query":"x"}`, "schema"},
		{"no schema_version", `{"schema":"mora.companion.context.request","mode":"think","query":"x"}`, "schema_version"},
		{"wrong schema_version", `{"schema":"mora.companion.context.request","schema_version":2,"mode":"think","query":"x"}`, "schema_version"},
		{"null envelope", `{"schema":null,"schema_version":null,"mode":"think","query":"x"}`, "schema"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := reader.calls
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, request(http.MethodPost, RouteContext, token, strings.NewReader(tc.body)))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s = %d, want 400\n%s", tc.name, rec.Code, rec.Body.String())
			}
			if reader.calls != before {
				t.Fatalf("%s reached the kernel", tc.name)
			}
			// The N02 rejection envelope: the code and the field path, and
			// never the value that failed.
			var refusal struct {
				Error string `json:"error"`
				Field string `json:"field"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &refusal); err != nil {
				t.Fatalf("%s: the refusal is not JSON: %v\n%s", tc.name, err, rec.Body.String())
			}
			if refusal.Error != CodeSchemaMismatch {
				t.Fatalf("%s: error = %q, want %q\n%s", tc.name, refusal.Error, CodeSchemaMismatch, rec.Body.String())
			}
			if refusal.Field != tc.wantField {
				t.Fatalf("%s: field = %q, want %q", tc.name, refusal.Field, tc.wantField)
			}
		})
	}

	// And the fully-enveloped form still works, so the refusal is the envelope
	// check and not a broken decoder.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, request(http.MethodPost, RouteContext, token, contextBody(t, ModeThink, "x")))
	if rec.Code != http.StatusOK {
		t.Fatalf("a fully enveloped request got %d, want 200\n%s", rec.Code, rec.Body.String())
	}
}

// TestServerRequiresTheBearerScheme drives a VALID token with no scheme.
//
// The earlier no-scheme case used an invalid token, so it proved nothing about
// the scheme: it would have passed against a parser that accepted a bare token,
// because the token was wrong either way.
func TestServerRequiresTheBearerScheme(t *testing.T) {
	srv, reader, _, token, _ := testServer(t)
	handler := srv.Handler()

	for _, tc := range []struct {
		name   string
		header string
		want   int
	}{
		{"bare valid token", token, http.StatusUnauthorized},
		{"wrong scheme, valid token", "Token " + token, http.StatusUnauthorized},
		{"basic scheme, valid token", "Basic " + token, http.StatusUnauthorized},
		{"no space after the scheme", "Bearer" + token, http.StatusUnauthorized},
		{"bearer, valid token", "Bearer " + token, http.StatusOK},
		{"lowercase bearer is still the scheme", "bearer " + token, http.StatusOK},
		{"mixed-case bearer is still the scheme", "BeArEr " + token, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := reader.calls
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, RouteHealth, nil)
			req.Host = "127.0.0.1:7778"
			req.Header.Set("Authorization", tc.header)
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("%s = %d, want %d\n%s", tc.name, rec.Code, tc.want, rec.Body.String())
			}
			reached := reader.calls != before
			if tc.want == http.StatusUnauthorized && reached {
				t.Fatalf("%s reached the kernel", tc.name)
			}
			if tc.want == http.StatusOK && !reached {
				t.Fatalf("%s did not reach the kernel", tc.name)
			}
			if tc.want == http.StatusUnauthorized && rec.Body.String() != unauthorizedBody {
				t.Fatalf("%s is distinguishable from every other refusal:\n%s", tc.name, rec.Body.String())
			}
		})
	}
}

// TestServerRefusesASecondConcurrentKernelCall is the work budget.
//
// Today walks the vault and context runs retrieval; neither is cheap, and a
// device that pipelines requests would multiply that by however many sockets it
// opens. The second caller is refused IMMEDIATELY rather than queued, because a
// queue in front of an expensive read holds sockets and memory for a caller who
// has already given up.
func TestServerRefusesASecondConcurrentKernelCall(t *testing.T) {
	srv, reader, _, token, _ := testServer(t)
	handler := srv.Handler()
	reader.entered = make(chan struct{}, 1)
	reader.hold = make(chan struct{})

	first := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, request(http.MethodGet, RouteToday, token, nil))
		first <- rec.Code
	}()

	select {
	case <-reader.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the first request never reached the kernel")
	}

	// The first call is pinned inside the kernel. The second must be refused
	// without waiting for it, and must not reach the kernel at all.
	before := reader.calls
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, request(http.MethodGet, RouteToday, token, nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("the second concurrent request = %d, want 503\n%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Fatal("the 503 does not say when to come back")
	}
	if reader.calls != before {
		t.Fatal("the refused request still reached the kernel")
	}

	close(reader.hold)
	if code := <-first; code != http.StatusOK {
		t.Fatalf("the first request = %d, want 200", code)
	}

	// The budget is released, so the next request is served rather than
	// permanently refused.
	reader.hold = nil
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, request(http.MethodGet, RouteToday, token, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("after the budget was released, a request got %d, want 200", rec.Code)
	}
}

// TestServerBoundsAKernelCallWithADeadline proves a slow read answers rather
// than hanging.
//
// The stub waits on the context, which is what a real read does when its
// deadline fires. The assertion is that the socket gets a bounded, typed refusal
// — not that the read is fast.
func TestServerBoundsAKernelCallWithADeadline(t *testing.T) {
	srv, reader, _, token, _ := testServer(t)
	reader.honorContext = true

	// Reach the handler directly with a context that has already expired, so
	// the test asserts the deadline PATH without waiting KernelTimeout for it.
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		srv.handleToday(rec, request(http.MethodGet, RouteToday, token, nil).WithContext(ctx))
		done <- rec
	}()

	var rec *httptest.ResponseRecorder
	select {
	case rec = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the handler hung past its own deadline")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("a request past its deadline = %d, want 503\n%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Fatal("the timeout does not say when to come back")
	}
	if !strings.Contains(rec.Body.String(), `"timeout"`) {
		t.Fatalf("the timeout is indistinguishable from any other outage:\n%s", rec.Body.String())
	}
	// And it carries nothing of the kernel's own error.
	if strings.Contains(rec.Body.String(), "context") {
		t.Fatalf("the timeout leaked the kernel error:\n%s", rec.Body.String())
	}
}

// TestServerReleasesTheBudgetOnEveryExit pins the release path for the two ways
// a handler can leave early. A budget that leaks on the error path is a server
// that answers 503 forever after its first failure.
func TestServerReleasesTheBudgetOnEveryExit(t *testing.T) {
	srv, reader, _, token, _ := testServer(t)
	handler := srv.Handler()

	reader.todayErr = errors.New("the kernel is unhappy")
	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, request(http.MethodGet, RouteToday, token, nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("failing request %d = %d, want 503", i, rec.Code)
		}
	}
	// A malformed body leaves through the rejection path, which is after the
	// budget was taken.
	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, request(http.MethodPost, RouteContext, token, strings.NewReader("{")))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("malformed request %d = %d, want 400", i, rec.Code)
		}
	}
	reader.todayErr = nil
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, request(http.MethodGet, RouteToday, token, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("after six early exits a healthy request got %d; the budget leaked", rec.Code)
	}
}

// TestServerAnswersTheThreeProjections pins the happy path and the envelope.
func TestServerAnswersTheThreeProjections(t *testing.T) {
	srv, reader, _, token, _ := testServer(t)
	handler := srv.Handler()

	for _, tc := range []struct {
		name, method, path, schema string
		body                       func() io.Reader
	}{
		{"today", http.MethodGet, RouteToday, SchemaToday, func() io.Reader { return nil }},
		{"health", http.MethodGet, RouteHealth, SchemaHealth, func() io.Reader { return nil }},
		{"context", http.MethodPost, RouteContext, SchemaContext, func() io.Reader { return contextBody(t, ModeSearch, "launch") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, request(tc.method, tc.path, token, tc.body()))
			if rec.Code != http.StatusOK {
				t.Fatalf("%s got %d, want 200\n%s", tc.name, rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
				t.Fatalf("%s content type = %q", tc.name, got)
			}
			var envelope struct {
				Schema  string `json:"schema"`
				Version int    `json:"schema_version"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
				t.Fatalf("%s body is not JSON: %v", tc.name, err)
			}
			if envelope.Schema != tc.schema || envelope.Version != SchemaVersion {
				t.Fatalf("%s envelope = %q v%d, want %q v%d", tc.name, envelope.Schema, envelope.Version, tc.schema, SchemaVersion)
			}
		})
	}

	if reader.lastReq.Mode != ModeSearch || reader.lastReq.Query != "launch" {
		t.Fatalf("the kernel saw mode %q query %q", reader.lastReq.Mode, reader.lastReq.Query)
	}
}

// TestServerRefusesToShipAnInvalidProjection is the outbound-validation gate. A
// kernel bug must become a 500, never a projection a phone would render: an item
// with no evidence is a claim with nothing behind it.
func TestServerRefusesToShipAnInvalidProjection(t *testing.T) {
	srv, reader, _, token, _ := testServer(t)
	reader.today.Items = []TodayItem{{ID: "itm_1", Kind: ItemChanged, Title: "Uncited", Evidence: []Evidence{}}}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, request(http.MethodGet, RouteToday, token, nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("an evidence-less Today item got %d, want 500", rec.Code)
	}
}

// TestServerSaysUnavailableWhenTheKernelFails keeps a kernel error opaque: the
// error text can carry a vault path, and a device has no use for it.
func TestServerSaysUnavailableWhenTheKernelFails(t *testing.T) {
	srv, reader, _, token, _ := testServer(t)
	reader.todayErr = errors.New("open /Users/someone/vault/mora/index.db: permission denied")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, request(http.MethodGet, RouteToday, token, nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("a failing kernel got %d, want 503", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "vault") || strings.Contains(rec.Body.String(), "permission denied") {
		t.Fatalf("the 503 leaked the kernel error:\n%s", rec.Body.String())
	}
}

// TestServer200NeverImpliesFresh is the honesty gate. Every route answers 200
// while the kernel reports a failed connector and a degraded index, and every
// answer carries that state in the body.
func TestServer200NeverImpliesFresh(t *testing.T) {
	srv, reader, _, token, _ := testServer(t)

	failed := SourceFreshness{Key: "gmail:work", State: FreshnessFailed, AgeSeconds: -1, ErrorCode: ErrAuthExpired}
	reader.today.Health = HealthSummary{State: HealthUnhealthy, Policy: PolicyReadonly}
	reader.today.Freshness = []SourceFreshness{failed}
	reader.context.Health = HealthSummary{State: HealthUnhealthy, Policy: PolicyReadonly}
	reader.context.Freshness = []SourceFreshness{failed}
	reader.health.State = HealthUnhealthy
	reader.health.Index = IndexHealth{State: HealthUnhealthy, Memories: 0}
	reader.health.Sources = []SourceFreshness{failed}

	handler := srv.Handler()
	for _, tc := range []struct {
		name, method, path string
		body               func() io.Reader
	}{
		{"today", http.MethodGet, RouteToday, func() io.Reader { return nil }},
		{"health", http.MethodGet, RouteHealth, func() io.Reader { return nil }},
		{"context", http.MethodPost, RouteContext, func() io.Reader { return contextBody(t, ModeThink, "q") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, request(tc.method, tc.path, token, tc.body()))
			if rec.Code != http.StatusOK {
				t.Fatalf("%s got %d, want 200 — an outage is reported in the body, not the status", tc.name, rec.Code)
			}
			body := rec.Body.String()
			if !strings.Contains(body, `"state": "failed"`) || !strings.Contains(body, `"error_code": "auth_expired"`) {
				t.Fatalf("%s answered 200 without carrying the failed source:\n%s", tc.name, body)
			}
			if !strings.Contains(body, `"unhealthy"`) {
				t.Fatalf("%s answered 200 without carrying the unhealthy state:\n%s", tc.name, body)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Silence
// ---------------------------------------------------------------------------

// TestServerLogsNothingPerRequest is the leak gate. A full authenticated
// exchange — a token, a query, a body and an answer — must leave the log
// untouched, and so must every refusal.
func TestServerLogsNothingPerRequest(t *testing.T) {
	srv, _, _, token, log := testServer(t)
	handler := srv.Handler()

	secret := "a question I would not want in a log file"
	for _, req := range []*http.Request{
		request(http.MethodGet, RouteToday, token, nil),
		request(http.MethodGet, RouteHealth, token, nil),
		request(http.MethodPost, RouteContext, token, contextBody(t, ModeThink, secret)),
		request(http.MethodGet, RouteToday, "wrong-token", nil),
		request(http.MethodGet, "/call", token, nil),
	} {
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}
	if log.Len() != 0 {
		t.Fatalf("the listener logged during a request:\n%s", log.String())
	}
}

// TestServerStartupBannerCarriesNoCredential pins what the ONE thing this
// listener does print may contain.
func TestServerStartupBannerCarriesNoCredential(t *testing.T) {
	reg, _, _, _ := testRegistry(t)
	token, _ := pairAndConfirm(t, reg, "phone")
	// Serve runs on another goroutine and writes the banner from there, so the
	// buffer the test polls has to be synchronized or -race reports the test's
	// own read against it.
	log := &lockedBuffer{}
	srv, err := NewServer(ServerOptions{Addr: "127.0.0.1:0", Devices: reg, Reader: newStubReader(), Log: log})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for log.String() == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Serve: %v", err)
	}
	banner := log.String()
	if !strings.Contains(banner, "127.0.0.1") || !strings.Contains(banner, "loopback only") {
		t.Fatalf("the banner does not say where it is listening:\n%s", banner)
	}
	if strings.Contains(banner, token) {
		t.Fatal("the startup banner printed a device token")
	}
}

// ---------------------------------------------------------------------------
// last_seen_at
// ---------------------------------------------------------------------------

// TestServerStampsLastSeenAtOnceAndNeverFailsAReadForIt pins the throttle and
// the best-effort contract: a device's audit stamp must not serialize its
// traffic, and a stamp that cannot be written must not cost the device its
// answer.
func TestServerStampsLastSeenAtOnceAndNeverFailsAReadForIt(t *testing.T) {
	srv, _, reg, token, _ := testServer(t)
	handler := srv.Handler()

	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, request(http.MethodGet, RouteHealth, token, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d got %d", i, rec.Code)
		}
	}
	devices, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if devices[0].LastSeenAt == "" {
		t.Fatal("the first request did not stamp last_seen_at")
	}

	// A registry that cannot stamp must not cost the device its answer.
	failing := &failingStamper{Registry: reg}
	broken, err := NewServer(ServerOptions{Addr: "127.0.0.1:7778", Devices: failing, Reader: newStubReader()})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	broken.Handler().ServeHTTP(rec, request(http.MethodGet, RouteHealth, token, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("a failed last-seen stamp cost the device its answer: %d", rec.Code)
	}
	if failing.calls == 0 {
		t.Fatal("the listener never tried to stamp last_seen_at")
	}
}

type failingStamper struct {
	*Registry
	calls int
}

func (f *failingStamper) MarkSeen(string) error {
	f.calls++
	return errors.New("registry locked")
}

// lockedBuffer is a bytes.Buffer a test can read while another goroutine writes.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
