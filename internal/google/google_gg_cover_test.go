package google

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"
	calendar "google.golang.org/api/calendar/v3"
	gmail "google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
)

// ============================================================================
// Shared test fakes (all gg-prefixed to avoid collisions with sibling workers).
// ============================================================================

// ggB64URL encodes s the way Gmail returns bodies (base64url, no padding).
func ggB64URL(s string) string {
	return base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString([]byte(s))
}

// ggFakeGoogle is a routable fake of the Gmail + Calendar REST APIs. Each handler
// field is nil unless a test wires it; a nil handler for a matched route yields a
// 404 so an unexpected call fails loudly rather than silently succeeding. Every
// request's URI is recorded so tests can assert query params (q, labelIds,
// pageToken, timeMin/timeMax) were actually sent.
type ggFakeGoogle struct {
	threadsList func(r *http.Request) (int, string)
	threadGet   func(id string, r *http.Request) (int, string)
	profile     func(r *http.Request) (int, string)
	labels      func(r *http.Request) (int, string)
	events      func(r *http.Request) (int, string)

	mu   sync.Mutex
	reqs []string
}

func (g *ggFakeGoogle) recorded() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.reqs...)
}

// lastMatching returns the first recorded request URI containing sub, or "".
func (g *ggFakeGoogle) lastMatching(sub string) string {
	for _, u := range g.recorded() {
		if strings.Contains(u, sub) {
			return u
		}
	}
	return ""
}

func (g *ggFakeGoogle) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.reqs = append(g.reqs, r.URL.RequestURI())
	g.mu.Unlock()

	path := r.URL.Path
	status, body := http.StatusNotFound, `{"error":{"code":404,"message":"unrouted"}}`
	switch {
	case strings.Contains(path, "/profile") && g.profile != nil:
		status, body = g.profile(r)
	case strings.Contains(path, "/labels") && g.labels != nil:
		status, body = g.labels(r)
	case strings.Contains(path, "/threads/") && g.threadGet != nil:
		id := path[strings.LastIndex(path, "/threads/")+len("/threads/"):]
		status, body = g.threadGet(id, r)
	case strings.HasSuffix(path, "/threads") && g.threadsList != nil:
		status, body = g.threadsList(r)
	case strings.Contains(path, "/events") && g.events != nil:
		status, body = g.events(r)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// ggServe starts the fake and returns its server (cleanup registered).
func ggServe(t *testing.T, g *ggFakeGoogle) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	return srv
}

// ggGmailSvc builds a real *gmail.Service whose base URL points at the fake, so
// no external network is ever touched.
func ggGmailSvc(t *testing.T, srv *httptest.Server) *gmail.Service {
	t.Helper()
	s, err := gmail.NewService(context.Background(),
		option.WithHTTPClient(srv.Client()),
		option.WithEndpoint(srv.URL))
	if err != nil {
		t.Fatalf("gmail.NewService: %v", err)
	}
	return s
}

func ggCalSvc(t *testing.T, srv *httptest.Server) *calendar.Service {
	t.Helper()
	s, err := calendar.NewService(context.Background(),
		option.WithHTTPClient(srv.Client()),
		option.WithEndpoint(srv.URL))
	if err != nil {
		t.Fatalf("calendar.NewService: %v", err)
	}
	return s
}

func TestGg_FetchPageContextCancelsGmailList(t *testing.T) {
	started, observed := make(chan struct{}), make(chan struct{})
	fake := &ggFakeGoogle{threadsList: func(r *http.Request) (int, string) {
		close(started)
		<-r.Context().Done()
		close(observed)
		return http.StatusRequestTimeout, `{}`
	}}
	srv := ggServe(t, fake)
	fetcher := &LiveFetcher{gmail: ggGmailSvc(t, srv), cal: ggCalSvc(t, srv)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := fetcher.FetchPageContext(ctx, KindGmailThread, FetchWindow{}, ""); done <- err }()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("gmail list cancellation = %v", err)
	}
	select {
	case <-observed:
	case <-time.After(time.Second):
		t.Fatal("gmail handler did not observe request cancellation")
	}
}

func TestGg_FetchPageContextCancelsGmailThreadDetail(t *testing.T) {
	started, observed := make(chan struct{}), make(chan struct{})
	fake := &ggFakeGoogle{
		threadsList: func(*http.Request) (int, string) {
			return http.StatusOK, `{"threads":[{"id":"t1"},{"id":"t2"}]}`
		},
		threadGet: func(_ string, r *http.Request) (int, string) {
			close(started)
			<-r.Context().Done()
			close(observed)
			return http.StatusRequestTimeout, `{}`
		},
	}
	srv := ggServe(t, fake)
	fetcher := &LiveFetcher{gmail: ggGmailSvc(t, srv), cal: ggCalSvc(t, srv)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := fetcher.FetchPageContext(ctx, KindGmailThread, FetchWindow{}, ""); done <- err }()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("gmail detail cancellation = %v", err)
	}
	<-observed
	if got := len(fake.recorded()); got != 2 {
		t.Fatalf("requests = %v, remaining detail loop continued", fake.recorded())
	}
}

func TestGg_FetchPageContextCancelsCalendarList(t *testing.T) {
	started, observed := make(chan struct{}), make(chan struct{})
	gmailCalls := 0
	fake := &ggFakeGoogle{
		threadsList: func(*http.Request) (int, string) { gmailCalls++; return http.StatusOK, `{}` },
		events: func(r *http.Request) (int, string) {
			close(started)
			<-r.Context().Done()
			close(observed)
			return http.StatusRequestTimeout, `{}`
		},
	}
	srv := ggServe(t, fake)
	fetcher := &LiveFetcher{gmail: ggGmailSvc(t, srv), cal: ggCalSvc(t, srv)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := fetcher.FetchPageContext(ctx, KindCalEvent, FetchWindow{}, ""); done <- err }()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("calendar cancellation = %v", err)
	}
	<-observed
	if gmailCalls != 0 {
		t.Fatalf("calendar cancellation touched Gmail %d time(s)", gmailCalls)
	}
}

func ggMustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// ggRoundTripFunc adapts a func into an http.RoundTripper for stubbing
// http.DefaultClient in the RevokeToken tests.
type ggRoundTripFunc func(*http.Request) (*http.Response, error)

func (f ggRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// ggSyncBuf is a concurrency-safe io.Writer + reader used to capture what
// StartLoopbackAuth prints while it runs in a goroutine.
type ggSyncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *ggSyncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *ggSyncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// ============================================================================
// oauth.go — IsConfigured / configFromInstalledJSON / LoadToken / saveTokenAt
// ============================================================================

func TestGg_IsConfigured(t *testing.T) {
	// Placeholder / no BYO creds => not configured.
	t.Setenv("MORA_GOOGLE_CREDENTIALS", "")
	if IsConfigured() {
		t.Fatal("placeholder embedded creds must report NOT configured")
	}
	// Real BYO creds => configured.
	p := filepath.Join(t.TempDir(), "byo.json")
	if err := os.WriteFile(p, []byte(`{"installed":{"client_id":"real.apps.googleusercontent.com","client_secret":"s","auth_uri":"https://a","token_uri":"https://t"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MORA_GOOGLE_CREDENTIALS", p)
	if !IsConfigured() {
		t.Fatal("valid BYO creds must report configured")
	}
}

func TestGg_ConfigFromInstalledJSONInvalid(t *testing.T) {
	// Malformed JSON must surface the unmarshal error, not silently build a config.
	cfg, err := configFromInstalledJSON([]byte("{not valid json"), []string{"scope"})
	if err == nil {
		t.Fatalf("malformed client JSON must error, got %+v", cfg)
	}
	if cfg != nil {
		t.Fatalf("config must be nil on unmarshal error, got %+v", cfg)
	}
}

func TestGg_ConfigFromInstalledJSONValidBuildsEndpoint(t *testing.T) {
	raw := []byte(`{"installed":{"client_id":"x.apps.googleusercontent.com","client_secret":"sec","auth_uri":"https://accounts.example/auth","token_uri":"https://oauth.example/token"}}`)
	cfg, err := configFromInstalledJSON(raw, []string{"a", "b"})
	if err != nil {
		t.Fatalf("valid creds: %v", err)
	}
	if cfg.ClientID != "x.apps.googleusercontent.com" || cfg.ClientSecret != "sec" {
		t.Fatalf("client id/secret not carried: %+v", cfg)
	}
	if cfg.Endpoint.AuthURL != "https://accounts.example/auth" || cfg.Endpoint.TokenURL != "https://oauth.example/token" {
		t.Fatalf("endpoint not built from installed JSON: %+v", cfg.Endpoint)
	}
}

func TestGg_LoadTokenCorruptJSON(t *testing.T) {
	p := filepath.Join(t.TempDir(), "google.json")
	if err := os.WriteFile(p, []byte("{ this is not a token"), 0o600); err != nil {
		t.Fatal(err)
	}
	tok, err := LoadToken(p)
	if err == nil {
		t.Fatalf("corrupt token JSON must error, got %+v", tok)
	}
}

func TestGg_SaveTokenAtErrorPaths(t *testing.T) {
	now := time.Date(2026, 6, 15, 8, 30, 0, 0, time.UTC)
	tok := &oauth2.Token{AccessToken: "a", RefreshToken: "r"}

	t.Run("mkdirall fails when a path component is a file", func(t *testing.T) {
		dir := t.TempDir()
		blocker := filepath.Join(dir, "blocker")
		if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		// blocker is a file, so MkdirAll(blocker/tokens) must fail.
		err := saveTokenAt(filepath.Join(blocker, "tokens", "google.json"), tok, now)
		if err == nil {
			t.Fatal("saveTokenAt must fail when parent dir cannot be created")
		}
	})

	t.Run("writefile fails when temp path is a directory", func(t *testing.T) {
		dir := t.TempDir()
		// Pre-create google.json.tmp as a DIRECTORY so the atomic-write step fails.
		if err := os.MkdirAll(filepath.Join(dir, "google.json.tmp"), 0o700); err != nil {
			t.Fatal(err)
		}
		err := saveTokenAt(filepath.Join(dir, "google.json"), tok, now)
		if err == nil {
			t.Fatal("saveTokenAt must fail when the temp file path is a directory")
		}
	})

	t.Run("rename fails onto a non-empty directory", func(t *testing.T) {
		dir := t.TempDir()
		// Pre-create google.json as a NON-EMPTY directory: WriteFile(tmp) succeeds
		// but Rename(tmp -> google.json) fails.
		target := filepath.Join(dir, "google.json")
		if err := os.MkdirAll(target, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(target, "keep"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		err := saveTokenAt(target, tok, now)
		if err == nil {
			t.Fatal("saveTokenAt must fail when rename target is a non-empty directory")
		}
	})
}

func TestGg_IsWSL(t *testing.T) {
	// On the darwin test host, IsWSL short-circuits false at the GOOS check.
	if runtime.GOOS != "linux" {
		if IsWSL() {
			t.Fatalf("IsWSL must be false on %s", runtime.GOOS)
		}
	}
}

// ============================================================================
// oauth.go — RevokeToken (http.DefaultClient stubbed; no real network)
// ============================================================================

func TestGg_RevokeToken(t *testing.T) {
	t.Run("success posts to revoke endpoint with the refresh token", func(t *testing.T) {
		var gotMethod, gotURL, gotContentType string
		old := http.DefaultClient
		http.DefaultClient = &http.Client{Transport: ggRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			gotMethod = r.Method
			gotURL = r.URL.String()
			gotContentType = r.Header.Get("Content-Type")
			return &http.Response{
				StatusCode: 200,
				Body:       io.NopCloser(strings.NewReader("")),
				Header:     make(http.Header),
			}, nil
		})}
		defer func() { http.DefaultClient = old }()

		if err := RevokeToken(context.Background(), &oauth2.Token{RefreshToken: "rt-abc"}); err != nil {
			t.Fatalf("RevokeToken: %v", err)
		}
		if gotMethod != "POST" {
			t.Fatalf("method = %q, want POST", gotMethod)
		}
		if !strings.Contains(gotURL, "oauth2.googleapis.com/revoke") || !strings.Contains(gotURL, "token=rt-abc") {
			t.Fatalf("revoke URL = %q, want revoke endpoint with token", gotURL)
		}
		if gotContentType != "application/x-www-form-urlencoded" {
			t.Fatalf("content-type = %q", gotContentType)
		}
	})

	t.Run("transport error is returned", func(t *testing.T) {
		old := http.DefaultClient
		http.DefaultClient = &http.Client{Transport: ggRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			return nil, errors.New("boom-network")
		})}
		defer func() { http.DefaultClient = old }()

		err := RevokeToken(context.Background(), &oauth2.Token{RefreshToken: "rt"})
		if err == nil || !strings.Contains(err.Error(), "boom-network") {
			t.Fatalf("want transport error surfaced, got %v", err)
		}
	})

	t.Run("request build error on unparseable token", func(t *testing.T) {
		// A control char in the refresh token makes the request URL unparseable, so
		// http.NewRequestWithContext fails before any network call.
		err := RevokeToken(context.Background(), &oauth2.Token{RefreshToken: "bad\ntoken"})
		if err == nil {
			t.Fatal("control char in refresh token must fail request construction")
		}
	})
}

// ============================================================================
// oauth.go — openBrowser
// ============================================================================

func TestGg_OpenBrowser(t *testing.T) {
	var opener string
	switch runtime.GOOS {
	case "darwin":
		opener = "open"
	case "linux":
		opener = "xdg-open"
	default:
		t.Skipf("openBrowser has no opener branch on %s", runtime.GOOS)
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "opened.txt")
	// A fake opener on PATH records the URL it was handed, proving openBrowser
	// forwards the auth URL to the platform opener (not just that it returns nil).
	script := "#!/bin/sh\nprintf '%s' \"$1\" > " + marker + "\n"
	if err := os.WriteFile(filepath.Join(dir, opener), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	want := "http://example.test/consent?state=xyz"
	if err := openBrowser(want); err != nil {
		t.Fatalf("openBrowser: %v", err)
	}
	var got string
	for i := 0; i < 400; i++ {
		if b, err := os.ReadFile(marker); err == nil && len(b) > 0 {
			got = string(b)
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got != want {
		t.Fatalf("opener received %q, want %q", got, want)
	}
}

// ============================================================================
// oauth.go — StartLoopbackAuth (full callback flow via httptest token endpoint)
// ============================================================================

// ggParseAuthURL scans printed output for the auth URL and returns its state and
// redirect_uri query params.
func ggParseAuthURL(t *testing.T, out string) (state, redirect string) {
	t.Helper()
	for _, tok := range strings.Fields(out) {
		u, err := url.Parse(tok)
		if err != nil {
			continue
		}
		q := u.Query()
		if s := q.Get("state"); s != "" {
			return s, q.Get("redirect_uri")
		}
	}
	t.Fatalf("no auth URL with a state param found in output:\n%s", out)
	return "", ""
}

// ggTokenServer returns an httptest server that answers OAuth token-exchange POSTs
// with the supplied JSON body and status.
func ggTokenServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func ggLoopbackCfg(tokenURL string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     "cid",
		ClientSecret: "secret",
		Scopes:       []string{"scope"},
		Endpoint: oauth2.Endpoint{
			AuthURL:  "http://auth.example.test/authorize",
			TokenURL: tokenURL,
		},
	}
}

// ggAwaitURL polls buf until the printed auth URL is available, then returns
// (state, redirect).
func ggAwaitURL(t *testing.T, buf *ggSyncBuf) (string, string) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if strings.Contains(buf.String(), "state=") {
			return ggParseAuthURL(t, buf.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("auth URL never printed:\n%s", buf.String())
	return "", ""
}

type ggAuthOutcome struct {
	tok *oauth2.Token
	err error
}

func TestGg_StartLoopbackAuthSuccess(t *testing.T) {
	t.Setenv("PATH", "") // neutralize openBrowser (no `open`/`xdg-open` on PATH)
	tokSrv := ggTokenServer(t, 200,
		`{"access_token":"at","token_type":"Bearer","refresh_token":"rt-live","expires_in":3600}`)
	cfg := ggLoopbackCfg(tokSrv.URL)

	buf := &ggSyncBuf{}
	res := make(chan ggAuthOutcome, 1)
	go func() {
		tok, err := StartLoopbackAuth(context.Background(), cfg, buf)
		res <- ggAuthOutcome{tok, err}
	}()

	state, redirect := ggAwaitURL(t, buf)
	// Drive the browser callback ourselves with the real state + an auth code.
	resp, err := http.Get(redirect + "?state=" + url.QueryEscape(state) + "&code=the-code")
	if err != nil {
		t.Fatalf("callback GET: %v", err)
	}
	_ = resp.Body.Close()

	select {
	case out := <-res:
		if out.err != nil {
			t.Fatalf("StartLoopbackAuth: %v", out.err)
		}
		if out.tok == nil || out.tok.RefreshToken != "rt-live" {
			t.Fatalf("token = %+v, want refresh token rt-live", out.tok)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("StartLoopbackAuth did not complete after callback")
	}
	// The printed guidance must lead with the browser-opening expectation.
	if !strings.Contains(buf.String(), "Opening your browser") {
		t.Fatalf("expected browser-opening guidance, got:\n%s", buf.String())
	}
}

func TestGg_StartLoopbackAuthStateMismatch(t *testing.T) {
	t.Setenv("PATH", "")
	tokSrv := ggTokenServer(t, 200, `{}`) // never reached
	cfg := ggLoopbackCfg(tokSrv.URL)

	buf := &ggSyncBuf{}
	res := make(chan ggAuthOutcome, 1)
	go func() {
		tok, err := StartLoopbackAuth(context.Background(), cfg, buf)
		res <- ggAuthOutcome{tok, err}
	}()

	_, redirect := ggAwaitURL(t, buf)
	resp, err := http.Get(redirect + "?state=WRONG&code=x")
	if err != nil {
		t.Fatalf("callback GET: %v", err)
	}
	_ = resp.Body.Close()

	select {
	case out := <-res:
		if out.err == nil || !strings.Contains(out.err.Error(), "state mismatch") {
			t.Fatalf("want state mismatch error, got tok=%+v err=%v", out.tok, out.err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("StartLoopbackAuth did not return on state mismatch")
	}
}

func TestGg_StartLoopbackAuthProviderError(t *testing.T) {
	t.Setenv("PATH", "")
	cfg := ggLoopbackCfg(ggTokenServer(t, 200, `{}`).URL)

	buf := &ggSyncBuf{}
	res := make(chan ggAuthOutcome, 1)
	go func() {
		tok, err := StartLoopbackAuth(context.Background(), cfg, buf)
		res <- ggAuthOutcome{tok, err}
	}()

	state, redirect := ggAwaitURL(t, buf)
	// Correct state but an OAuth error param (e.g. user denied consent).
	resp, err := http.Get(redirect + "?state=" + url.QueryEscape(state) + "&error=access_denied")
	if err != nil {
		t.Fatalf("callback GET: %v", err)
	}
	_ = resp.Body.Close()

	select {
	case out := <-res:
		if out.err == nil || !strings.Contains(out.err.Error(), "access_denied") {
			t.Fatalf("want oauth error surfaced, got tok=%+v err=%v", out.tok, out.err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("StartLoopbackAuth did not return on provider error")
	}
}

func TestGg_StartLoopbackAuthNoRefreshToken(t *testing.T) {
	t.Setenv("PATH", "")
	// Token endpoint returns a valid token WITHOUT a refresh token.
	tokSrv := ggTokenServer(t, 200, `{"access_token":"at","token_type":"Bearer","expires_in":3600}`)
	cfg := ggLoopbackCfg(tokSrv.URL)

	buf := &ggSyncBuf{}
	res := make(chan ggAuthOutcome, 1)
	go func() {
		tok, err := StartLoopbackAuth(context.Background(), cfg, buf)
		res <- ggAuthOutcome{tok, err}
	}()

	state, redirect := ggAwaitURL(t, buf)
	resp, err := http.Get(redirect + "?state=" + url.QueryEscape(state) + "&code=the-code")
	if err != nil {
		t.Fatalf("callback GET: %v", err)
	}
	_ = resp.Body.Close()

	select {
	case out := <-res:
		if out.err == nil || !strings.Contains(out.err.Error(), "no refresh token") {
			t.Fatalf("want no-refresh-token error, got tok=%+v err=%v", out.tok, out.err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("StartLoopbackAuth did not return")
	}
}

func TestGg_StartLoopbackAuthExchangeError(t *testing.T) {
	t.Setenv("PATH", "")
	// Token endpoint rejects the exchange.
	tokSrv := ggTokenServer(t, 400, `{"error":"invalid_grant"}`)
	cfg := ggLoopbackCfg(tokSrv.URL)

	buf := &ggSyncBuf{}
	res := make(chan ggAuthOutcome, 1)
	go func() {
		tok, err := StartLoopbackAuth(context.Background(), cfg, buf)
		res <- ggAuthOutcome{tok, err}
	}()

	state, redirect := ggAwaitURL(t, buf)
	resp, err := http.Get(redirect + "?state=" + url.QueryEscape(state) + "&code=the-code")
	if err != nil {
		t.Fatalf("callback GET: %v", err)
	}
	_ = resp.Body.Close()

	select {
	case out := <-res:
		if out.err == nil || !strings.Contains(out.err.Error(), "token exchange") {
			t.Fatalf("want token-exchange error, got tok=%+v err=%v", out.tok, out.err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("StartLoopbackAuth did not return on exchange error")
	}
}

func TestGg_StartLoopbackAuthContextCancelled(t *testing.T) {
	t.Setenv("PATH", "")
	cfg := ggLoopbackCfg(ggTokenServer(t, 200, `{}`).URL)

	ctx, cancel := context.WithCancel(context.Background())
	buf := &ggSyncBuf{}
	res := make(chan ggAuthOutcome, 1)
	go func() {
		tok, err := StartLoopbackAuth(ctx, cfg, buf)
		res <- ggAuthOutcome{tok, err}
	}()
	// Wait until it is blocked on the select, then cancel (no callback arrives).
	ggAwaitURL(t, buf)
	cancel()

	select {
	case out := <-res:
		if !errors.Is(out.err, context.Canceled) {
			t.Fatalf("want context.Canceled, got tok=%+v err=%v", out.tok, out.err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("StartLoopbackAuth did not observe context cancellation")
	}
}

// ============================================================================
// authlog.go — RecordAuth / LastAuth error + skip paths
// ============================================================================

func TestGg_RecordAuthErrorPaths(t *testing.T) {
	at := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	t.Run("mkdirall fails when dir path component is a file", func(t *testing.T) {
		dir := t.TempDir()
		blocker := filepath.Join(dir, "afile")
		if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := RecordAuth(filepath.Join(blocker, "sub"), "google", at); err == nil {
			t.Fatal("RecordAuth must fail when its dir cannot be created")
		}
	})

	t.Run("openfile fails when history path is a directory", func(t *testing.T) {
		dir := t.TempDir()
		// Make auth-history.jsonl a directory so O_WRONLY open fails.
		if err := os.MkdirAll(filepath.Join(dir, authHistoryFile), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := RecordAuth(dir, "google", at); err == nil {
			t.Fatal("RecordAuth must fail when the history file path is a directory")
		}
	})
}

func TestGg_LastAuthSkipsCorruptAndFiltersAccount(t *testing.T) {
	dir := t.TempDir()
	want := time.Date(2026, 6, 15, 8, 30, 0, 0, time.UTC)
	other := AuthEvent{Account: "other", At: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	match := AuthEvent{Account: "google", At: want}
	otherB, _ := json.Marshal(other)
	matchB, _ := json.Marshal(match)
	// Corrupt line + blank line + wrong-account line + matching line.
	content := "this is not json\n\n" + string(otherB) + "\n" + string(matchB) + "\n"
	if err := os.WriteFile(filepath.Join(dir, authHistoryFile), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	got, ok, err := LastAuth(dir, "google")
	if err != nil {
		t.Fatalf("LastAuth: %v", err)
	}
	if !ok {
		t.Fatal("expected the matching (google) event to be found despite corrupt/other lines")
	}
	if !got.Equal(want) {
		t.Fatalf("LastAuth = %v, want %v (corrupt/blank/other lines must be skipped)", got, want)
	}
}

func TestGg_LastAuthLineTooLong(t *testing.T) {
	dir := t.TempDir()
	// A single line larger than the scanner's 1 MiB max token must surface an
	// error rather than silently truncating the scan and hiding newer events.
	huge := bytes.Repeat([]byte("x"), 2<<20) // 2 MiB, no newline
	if err := os.WriteFile(filepath.Join(dir, authHistoryFile), huge, 0o600); err != nil {
		t.Fatal(err)
	}
	_, ok, err := LastAuth(dir, "google")
	if err == nil {
		t.Fatal("an over-long line must cause a scanner error, not a silent skip")
	}
	if ok {
		t.Fatal("no event should be reported found on a scan error")
	}
}

func TestGg_LastAuthOpenPermissionError(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, authHistoryFile)
	if err := os.WriteFile(p, []byte(`{"account":"google","at":"2026-06-15T08:30:00Z"}`+"\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	// Confirm the environment actually denies read (e.g. not running as root).
	if f, err := os.Open(p); err == nil {
		_ = f.Close()
		t.Skip("history file is readable despite 0000 perms (likely root); skipping permission-denied path")
	}
	_, ok, err := LastAuth(dir, "google")
	if err == nil {
		t.Fatal("a non-IsNotExist open error must be returned by LastAuth")
	}
	if os.IsNotExist(err) {
		t.Fatalf("error should be permission-denied, not not-exist: %v", err)
	}
	if ok {
		t.Fatal("no event should be found on open error")
	}
}

// ============================================================================
// identity.go — addHeader empty + add edge cases
// ============================================================================

func TestGg_AddrSetAddHeaderEmpty(t *testing.T) {
	s := newAddrSet()
	s.addHeader("")
	s.addHeader("   ")
	if !s.empty() {
		t.Fatalf("empty/whitespace headers must add no addresses, got %v", s.list())
	}
}

func TestGg_AddrSetAdd(t *testing.T) {
	s := newAddrSet()
	s.add("", "Nobody")             // empty address => ignored
	s.add("A@B.com", "First Name")  // lowercased, name recorded
	s.add("a@b.com", "Second Name") // duplicate addr => keeps FIRST name
	s.add("c@d.com", "")            // addr present, no name

	got := s.list()
	if len(got) != 2 || !has(got, "a@b.com") || !has(got, "c@d.com") {
		t.Fatalf("list = %v, want [a@b.com c@d.com]", got)
	}
	if s.names["a@b.com"] != "First Name" {
		t.Fatalf("names[a@b.com] = %q, want first name kept", s.names["a@b.com"])
	}
	if _, ok := s.names["c@d.com"]; ok {
		t.Fatalf("empty display name must not be recorded, got %q", s.names["c@d.com"])
	}
}

// ============================================================================
// gmail.go — buildGmailQuery / decodeGmailBody / gmailAttachments / stripQuoted
// gmailThreadToItem no-subject
// ============================================================================

func TestGg_BuildGmailQuery(t *testing.T) {
	base := "-category:promotions -category:social"

	if q := buildGmailQuery(FetchWindow{}); q != base {
		t.Fatalf("empty window q = %q, want %q", q, base)
	}

	since := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	q := buildGmailQuery(FetchWindow{Since: since, Query: "invoice"})
	if !strings.Contains(q, base) {
		t.Fatalf("q = %q, missing category filters", q)
	}
	if !strings.Contains(q, "after:2026/06/15") {
		t.Fatalf("q = %q, missing after: date from Since", q)
	}
	if !strings.Contains(q, "invoice") {
		t.Fatalf("q = %q, missing user query", q)
	}
}

func TestGg_DecodeGmailBody(t *testing.T) {
	if got := decodeGmailBody(nil); got != "" {
		t.Fatalf("nil part => %q, want empty", got)
	}
	// Direct text/plain part.
	plain := &gmail.MessagePart{
		MimeType: "text/plain",
		Body:     &gmail.MessagePartBody{Data: ggB64URL("hello body")},
	}
	if got := decodeGmailBody(plain); got != "hello body" {
		t.Fatalf("text/plain => %q", got)
	}
	// Nested: multipart wrapper whose child holds the text/plain.
	nested := &gmail.MessagePart{
		MimeType: "multipart/alternative",
		Parts: []*gmail.MessagePart{
			{MimeType: "text/html", Body: &gmail.MessagePartBody{Data: ggB64URL("<b>x</b>")}},
			{MimeType: "text/plain", Body: &gmail.MessagePartBody{Data: ggB64URL("nested plain")}},
		},
	}
	if got := decodeGmailBody(nested); got != "nested plain" {
		t.Fatalf("nested text/plain => %q", got)
	}
	// No text anywhere => empty.
	none := &gmail.MessagePart{MimeType: "multipart/mixed", Parts: []*gmail.MessagePart{
		{MimeType: "image/png", Body: &gmail.MessagePartBody{Data: ggB64URL("bin")}},
	}}
	if got := decodeGmailBody(none); got != "" {
		t.Fatalf("no text/plain => %q, want empty", got)
	}
}

func TestGg_GmailAttachments(t *testing.T) {
	if got := gmailAttachments(nil); got != nil {
		t.Fatalf("nil part => %v, want nil", got)
	}
	p := &gmail.MessagePart{
		MimeType: "multipart/mixed",
		Parts: []*gmail.MessagePart{
			{Filename: "report.pdf", MimeType: "application/pdf", Body: &gmail.MessagePartBody{Size: 42}},
			{Filename: "", MimeType: "text/plain", Body: &gmail.MessagePartBody{Size: 1}}, // no filename => skipped
			{Filename: "noBody.txt"}, // Filename set but Body nil => skipped
		},
	}
	got := gmailAttachments(p)
	if len(got) != 1 {
		t.Fatalf("attachments = %v, want exactly the one with a filename+body", got)
	}
	if got[0].Filename != "report.pdf" || got[0].MimeType != "application/pdf" || got[0].Size != 42 {
		t.Fatalf("attachment fields wrong: %+v", got[0])
	}
}

func TestGg_StripQuoted(t *testing.T) {
	body := "Real line one\n> quoted reply\nReal line two\nOn Mon, someone wrote:\nignored trailing\n> also ignored"
	got := stripQuoted(body)
	if !strings.Contains(got, "Real line one") || !strings.Contains(got, "Real line two") {
		t.Fatalf("stripQuoted dropped real content: %q", got)
	}
	if strings.Contains(got, "quoted reply") {
		t.Fatalf("stripQuoted kept a >-quoted line: %q", got)
	}
	if strings.Contains(got, "ignored trailing") || strings.Contains(got, "someone wrote") {
		t.Fatalf("stripQuoted kept content after the attribution line: %q", got)
	}
}

func TestGg_GmailThreadToItemNoSubject(t *testing.T) {
	// A thread whose only message has a From but NO Subject, and InternalDate 0
	// (so no occurred_at) must fall back to "(no subject)".
	th := &gmail.Thread{Id: "tX", Messages: []*gmail.Message{
		{InternalDate: 0, Payload: &gmail.MessagePart{Headers: []*gmail.MessagePartHeader{
			hdr("From", "solo@example.com"),
		}}},
	}}
	it := gmailThreadToItem(th)
	if it.Title != "(no subject)" {
		t.Fatalf("title = %q, want (no subject)", it.Title)
	}
	if !strings.Contains(it.Body, "From: solo@example.com") {
		t.Fatalf("body = %q, want From line", it.Body)
	}
	if _, present := it.Meta["occurred_at"]; present {
		t.Fatal("a thread with no message time must not emit occurred_at")
	}
}

// ============================================================================
// client.go — decodeBase64URL / NewLiveFetcher / FetchPage / AuthedEmail /
// AuthedLabels
// ============================================================================

func TestGg_DecodeBase64URL(t *testing.T) {
	// No-padding base64url (the primary path).
	if got := decodeBase64URL(ggB64URL("hello")); got != "hello" {
		t.Fatalf("no-pad decode = %q", got)
	}
	// Padded standard base64url => primary (no-pad) fails, fallback succeeds.
	padded := base64.URLEncoding.EncodeToString([]byte("world"))
	if !strings.HasSuffix(padded, "=") {
		t.Fatalf("test setup: expected padded input, got %q", padded)
	}
	if got := decodeBase64URL(padded); got != "world" {
		t.Fatalf("padded fallback decode = %q", got)
	}
	// Undecodable => empty string.
	if got := decodeBase64URL("!!!!not base64!!!!"); got != "" {
		t.Fatalf("invalid input => %q, want empty", got)
	}
}

func TestGg_NewLiveFetcher(t *testing.T) {
	// Service construction is lazy (no network). A valid config + token must yield
	// a usable fetcher.
	cfg := &oauth2.Config{ClientID: "c", Endpoint: oauth2.Endpoint{TokenURL: "https://t.example/token"}}
	f, err := NewLiveFetcher(context.Background(), cfg, &oauth2.Token{AccessToken: "a", RefreshToken: "r"})
	if err != nil {
		t.Fatalf("NewLiveFetcher: %v", err)
	}
	if f == nil {
		t.Fatal("NewLiveFetcher returned nil fetcher")
	}
}

func TestGg_FetchPageUnsupportedKind(t *testing.T) {
	f := ggNewLiveFetcher(nil, nil) // services never dereferenced for an unknown kind
	_, err := f.FetchPage(ItemKind("bogus"), FetchWindow{}, "")
	if err == nil || !strings.Contains(err.Error(), "unsupported kind") {
		t.Fatalf("want unsupported-kind error, got %v", err)
	}
}

func TestGg_AuthedEmail(t *testing.T) {
	t.Run("success lowercases and trims", func(t *testing.T) {
		g := &ggFakeGoogle{profile: func(r *http.Request) (int, string) {
			return 200, `{"emailAddress":"  Me@Example.COM  "}`
		}}
		f := ggNewLiveFetcher(ggGmailSvc(t, ggServe(t, g)), nil)
		got, err := f.AuthedEmail()
		if err != nil {
			t.Fatalf("AuthedEmail: %v", err)
		}
		if got != "me@example.com" {
			t.Fatalf("AuthedEmail = %q, want normalized me@example.com", got)
		}
	})
	t.Run("api error surfaces", func(t *testing.T) {
		g := &ggFakeGoogle{profile: func(r *http.Request) (int, string) {
			return 500, `{"error":{"code":500,"message":"boom"}}`
		}}
		f := ggNewLiveFetcher(ggGmailSvc(t, ggServe(t, g)), nil)
		if _, err := f.AuthedEmail(); err == nil {
			t.Fatal("AuthedEmail must surface an API error")
		}
	})
}

func TestGg_AuthedLabels(t *testing.T) {
	t.Run("success maps id->name", func(t *testing.T) {
		g := &ggFakeGoogle{labels: func(r *http.Request) (int, string) {
			return 200, `{"labels":[{"id":"L1","name":"Work"},{"id":"L2","name":"Personal"}]}`
		}}
		f := ggNewLiveFetcher(ggGmailSvc(t, ggServe(t, g)), nil)
		got, err := f.AuthedLabels()
		if err != nil {
			t.Fatalf("AuthedLabels: %v", err)
		}
		if got["L1"] != "Work" || got["L2"] != "Personal" {
			t.Fatalf("labels = %v", got)
		}
	})
	t.Run("api error surfaces", func(t *testing.T) {
		g := &ggFakeGoogle{labels: func(r *http.Request) (int, string) {
			return 403, `{"error":{"code":403,"message":"denied"}}`
		}}
		f := ggNewLiveFetcher(ggGmailSvc(t, ggServe(t, g)), nil)
		if _, err := f.AuthedLabels(); err == nil {
			t.Fatal("AuthedLabels must surface an API error")
		}
	})
}

// ============================================================================
// gmail.go — fetchGmailPage via FetchPage(KindGmailThread)
// ============================================================================

func TestGg_FetchGmailPage(t *testing.T) {
	t.Run("lists, fetches each thread, skips per-thread failures, sends query", func(t *testing.T) {
		full := &gmail.Thread{Id: "t1", Messages: []*gmail.Message{
			{InternalDate: 1700000000000, Payload: &gmail.MessagePart{
				MimeType: "text/plain",
				Headers: []*gmail.MessagePartHeader{
					hdr("Subject", "Invoice #7"),
					hdr("From", "Alice <alice@example.com>"),
					hdr("To", "bob@example.com"),
				},
				Body: &gmail.MessagePartBody{Data: ggB64URL("please pay")},
			}},
		}}
		g := &ggFakeGoogle{
			threadsList: func(r *http.Request) (int, string) {
				return 200, ggMustJSON(t, &gmail.ListThreadsResponse{
					Threads:       []*gmail.Thread{{Id: "t1"}, {Id: "t2"}},
					NextPageToken: "NEXT-CURSOR",
				})
			},
			threadGet: func(id string, r *http.Request) (int, string) {
				if id == "t2" {
					return 500, `{"error":{"code":500,"message":"thread boom"}}` // skipped
				}
				return 200, ggMustJSON(t, full)
			},
		}
		f := ggNewLiveFetcher(ggGmailSvc(t, ggServe(t, g)), nil)

		since := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
		page, err := f.FetchPage(KindGmailThread,
			FetchWindow{Since: since, Query: "invoice", Labels: []string{"INBOX", "IMPORTANT"}},
			"PAGE2")
		if err != nil {
			t.Fatalf("FetchPage(gmail): %v", err)
		}
		if len(page.Items) != 1 {
			t.Fatalf("items = %d, want 1 (t2 skipped on per-thread error)", len(page.Items))
		}
		if page.Items[0].Title != "Invoice #7" || page.Items[0].ProviderID != "t1" {
			t.Fatalf("mapped item wrong: %+v", page.Items[0])
		}
		if page.NextCursor != "NEXT-CURSOR" {
			t.Fatalf("next cursor = %q, want NEXT-CURSOR", page.NextCursor)
		}
		listReq := g.lastMatching("/threads?")
		if listReq == "" {
			listReq = g.lastMatching("threads?")
		}
		if listReq == "" {
			t.Fatalf("no threads-list request recorded: %v", g.recorded())
		}
		for _, want := range []string{"after%3A2026%2F06%2F15", "invoice", "INBOX", "IMPORTANT", "pageToken=PAGE2"} {
			if !strings.Contains(listReq, want) {
				t.Fatalf("list request %q missing %q", listReq, want)
			}
		}
	})

	t.Run("top-level list error is returned", func(t *testing.T) {
		g := &ggFakeGoogle{threadsList: func(r *http.Request) (int, string) {
			return 503, `{"error":{"code":503,"message":"unavailable"}}`
		}}
		f := ggNewLiveFetcher(ggGmailSvc(t, ggServe(t, g)), nil)
		if _, err := f.FetchPage(KindGmailThread, FetchWindow{}, ""); err == nil {
			t.Fatal("a threads-list API error must propagate")
		}
	})
}

// ============================================================================
// calendar.go — fetchCalendarPage via FetchPage / calEventToItem / parseCalTime
// ============================================================================

func TestGg_FetchCalendarPage(t *testing.T) {
	ev := &calendar.Event{
		Id:        "e1",
		Summary:   "Design sync",
		Location:  "Room 4",
		Start:     &calendar.EventDateTime{DateTime: "2026-06-04T09:00:00Z"},
		Attendees: []*calendar.EventAttendee{{Email: "adit@x.com"}},
	}

	t.Run("defaults calendar to primary and sends time window", func(t *testing.T) {
		g := &ggFakeGoogle{events: func(r *http.Request) (int, string) {
			return 200, ggMustJSON(t, &calendar.Events{Items: []*calendar.Event{ev}, NextPageToken: "NC"})
		}}
		f := ggNewLiveFetcher(nil, ggCalSvc(t, ggServe(t, g)))
		since := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
		until := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)
		page, err := f.FetchPage(KindCalEvent, FetchWindow{Since: since, Until: until}, "")
		if err != nil {
			t.Fatalf("FetchPage(calendar): %v", err)
		}
		if len(page.Items) != 1 || page.Items[0].ProviderID != "primary/e1" {
			t.Fatalf("items = %+v, want one primary/e1", page.Items)
		}
		if page.NextCursor != "NC" {
			t.Fatalf("next cursor = %q", page.NextCursor)
		}
		req := g.lastMatching("/events")
		if req == "" {
			t.Fatalf("no events request recorded: %v", g.recorded())
		}
		if !strings.Contains(req, "calendars/primary/events") {
			t.Fatalf("request %q did not default to the primary calendar", req)
		}
		if !strings.Contains(req, "timeMin=") || !strings.Contains(req, "timeMax=") {
			t.Fatalf("request %q missing timeMin/timeMax", req)
		}
	})

	t.Run("explicit calendar id and cursor are honored", func(t *testing.T) {
		g := &ggFakeGoogle{events: func(r *http.Request) (int, string) {
			return 200, ggMustJSON(t, &calendar.Events{Items: []*calendar.Event{ev}})
		}}
		f := ggNewLiveFetcher(nil, ggCalSvc(t, ggServe(t, g)))
		if _, err := f.FetchPage(KindCalEvent, FetchWindow{CalendarID: "team@grp"}, "CURSOR9"); err != nil {
			t.Fatalf("FetchPage: %v", err)
		}
		req := g.lastMatching("/events")
		if !strings.Contains(req, "calendars/team@grp/events") && !strings.Contains(req, "calendars/team%40grp/events") {
			t.Fatalf("request %q did not use explicit calendar id", req)
		}
		if !strings.Contains(req, "pageToken=CURSOR9") {
			t.Fatalf("request %q missing pageToken cursor", req)
		}
	})

	t.Run("api error is returned", func(t *testing.T) {
		g := &ggFakeGoogle{events: func(r *http.Request) (int, string) {
			return 500, `{"error":{"code":500,"message":"boom"}}`
		}}
		f := ggNewLiveFetcher(nil, ggCalSvc(t, ggServe(t, g)))
		if _, err := f.FetchPage(KindCalEvent, FetchWindow{}, ""); err == nil {
			t.Fatal("a calendar API error must propagate")
		}
	})
}

func TestGg_CalEventToItemVariants(t *testing.T) {
	t.Run("full event renders where/attendees/description and is not deleted", func(t *testing.T) {
		ev := &calendar.Event{
			Id:          "e1",
			Summary:     "Planning",
			Location:    "HQ / Room 2",
			Description: "Agenda: roadmap",
			Status:      "confirmed",
			Start:       &calendar.EventDateTime{DateTime: "2026-06-04T09:00:00Z"},
			Organizer:   &calendar.EventOrganizer{Email: "Boss@Corp.com", DisplayName: "Boss"},
			Attendees: []*calendar.EventAttendee{
				{Email: "a@x.com"}, {Email: "b@y.com", DisplayName: "Bee"},
			},
		}
		it := calEventToItem("primary", ev)
		if it.Title != "Planning" {
			t.Fatalf("title = %q", it.Title)
		}
		for _, want := range []string{"Where: HQ / Room 2", "Attendees: ", "Agenda: roadmap"} {
			if !strings.Contains(it.Body, want) {
				t.Fatalf("body %q missing %q", it.Body, want)
			}
		}
		if it.Deleted {
			t.Fatal("a confirmed event must not be marked deleted")
		}
		if org, _ := it.Meta["organizer"].(string); org != "boss@corp.com" {
			t.Fatalf("organizer = %q, want lowercased", org)
		}
	})

	t.Run("no summary falls back to (no title); cancelled => deleted", func(t *testing.T) {
		ev := &calendar.Event{
			Id:     "e2",
			Status: "cancelled",
			Start:  &calendar.EventDateTime{Date: "2026-06-04"},
			// no Summary, no Location, no Description, no Attendees, no Organizer
		}
		it := calEventToItem("cal2", ev)
		if it.Title != "(no title)" {
			t.Fatalf("title = %q, want (no title)", it.Title)
		}
		if !it.Deleted {
			t.Fatal("a cancelled event must be marked deleted")
		}
		if _, present := it.Meta["organizer"]; present {
			t.Fatal("no organizer must be emitted when absent")
		}
		if _, present := it.Meta["attendees"]; present {
			t.Fatal("no attendees key when there are none")
		}
	})
}

// TestGg_CalEventSourceCreatedAt pins the source clock the browse row reads as
// `source_created_at` (#218). Google's Event.Created is when the event came into
// existence at the provider — a different question from when it starts — so the
// two must land in Meta as two distinct, separately-derivable values. It is
// absent on some event kinds and is provider text either way, so it is normalized
// when it parses and OMITTED when it does not: an unparseable value published
// here becomes an unparseable timestamp on a browse row, and an empty one becomes
// content-hash material on every such event.
func TestGg_CalEventSourceCreatedAt(t *testing.T) {
	base := func(created string) *calendar.Event {
		return &calendar.Event{
			Id:      "e-created",
			Summary: "Board offsite",
			Created: created,
			Start:   &calendar.EventDateTime{DateTime: "2027-02-10T16:00:00Z"},
		}
	}

	t.Run("creation time is normalized and distinct from the start", func(t *testing.T) {
		// Created months before it starts, in a non-UTC zone: normalization to UTC
		// RFC3339 keeps Meta bytes stable across runs, as occurred_at already is.
		it := calEventToItem("primary", base("2026-07-26T07:30:00-04:00"))
		got, _ := it.Meta["source_created_at"].(string)
		if got != "2026-07-26T11:30:00Z" {
			t.Fatalf("meta[source_created_at] = %q, want the normalized creation instant", got)
		}
		start, _ := it.Meta["occurred_at"].(string)
		if start != "2027-02-10T16:00:00Z" {
			t.Fatalf("meta[occurred_at] = %q, want the event start", start)
		}
		if got == start {
			t.Fatal("creation time and start collapsed into one value — the two clocks must stay distinct")
		}
	})

	// A valid offset or fraction is honoured, not merely tolerated: the instant is
	// converted to UTC, so the offset has to have been read correctly to begin with.
	t.Run("valid offsets and fractional seconds are converted, not discarded", func(t *testing.T) {
		for _, tc := range []struct{ created, want string }{
			{"2026-07-26T11:30:00Z", "2026-07-26T11:30:00Z"},
			{"2026-07-26T17:00:00+05:30", "2026-07-26T11:30:00Z"},
			{"2026-07-26T11:30:00.123Z", "2026-07-26T11:30:00Z"},
			{"2026-07-26T07:30:00.999999-04:00", "2026-07-26T11:30:00Z"},
			{"2026-07-26T11:30:00-00:00", "2026-07-26T11:30:00Z"},
			{"2026-07-25T12:30:00-23:00", "2026-07-26T11:30:00Z"},
		} {
			it := calEventToItem("primary", base(tc.created))
			if got, _ := it.Meta["source_created_at"].(string); got != tc.want {
				t.Errorf("Created=%q => meta[source_created_at] = %q, want %q", tc.created, got, tc.want)
			}
		}
	})

	// Sabotage. Event.Created is provider TEXT, and the gate on it is strict
	// RFC 3339 rather than time.Parse — which matters more here than on a
	// pass-through field, because what lands in Meta is the UTC-NORMALIZED render.
	// A stamp Go tolerates does not produce a malformed value downstream; it
	// produces a well-formed value holding the WRONG instant, with nothing left to
	// detect it: `+00:60` reads as +01:00 and `+24:00` as a 24-hour zone, so the
	// persisted creation time would be off by an hour or a day. Text that is not
	// RFC 3339 is not evidence of a creation time, so the key is omitted.
	for _, tc := range []struct {
		name    string
		created string
	}{
		{"absent on birthdays and some imported feeds", ""},
		{"date-only is not an instant", "2026-07-26"},
		{"provider text that does not parse", "Jul 26, 2026 7:30am"},
		{"one-digit hour", "2026-07-26T7:30:00Z"},
		{"offset minute 60, which time.Parse folds into +01:00", "2026-07-26T07:30:00+00:60"},
		{"negative offset minute 60", "2026-07-26T07:30:00-00:60"},
		{"offset hour 24", "2026-07-26T07:30:00+24:00"},
		{"negative offset hour 24", "2026-07-26T07:30:00-24:00"},
		{"comma fractional separator", "2026-07-26T07:30:00,5Z"},
		{"fractional dot with no digits", "2026-07-26T07:30:00.Z"},
		{"fractional dot with no digits before an offset", "2026-07-26T07:30:00.-04:00"},
		{"trailing dot and no zone", "2026-07-26T07:30:00."},
		{"no zone at all", "2026-07-26T07:30:00"},
		{"offset without its colon", "2026-07-26T07:30:00+0100"},
		{"one-digit offset hour", "2026-07-26T07:30:00+1:00"},
		{"one-digit offset minute", "2026-07-26T07:30:00+01:0"},
		{"lowercase zone designator", "2026-07-26T07:30:00z"},
		{"lowercase date/time separator", "2026-07-26t07:30:00Z"},
		{"space instead of T", "2026-07-26 07:30:00Z"},
		{"trailing byte after a valid stamp", "2026-07-26T07:30:00Z "},
		{"leap second", "2026-07-26T07:30:60Z"},
		{"well-formed but not a real calendar date", "2026-02-30T07:30:00Z"},
	} {
		tc := tc
		t.Run("omitted when "+tc.name, func(t *testing.T) {
			it := calEventToItem("primary", base(tc.created))
			if v, present := it.Meta["source_created_at"]; present {
				t.Fatalf("meta[source_created_at] = %v for Created=%q, want the key omitted", v, tc.created)
			}
			// The event is otherwise unaffected — a missing creation clock never
			// costs the row its start.
			if start, _ := it.Meta["occurred_at"].(string); start != "2027-02-10T16:00:00Z" {
				t.Fatalf("meta[occurred_at] = %q, want the event start intact", start)
			}
		})
	}
}

var ggRFC3339GoTolerates = []struct{ stamp, why string }{
	{"2026-07-31T1:12:34Z", "one-digit hour"},
	{"2026-07-31T01:12:34+00:60", "offset minute 60, folded into +01:00"},
	{"2026-07-31T01:12:34-00:60", "offset minute 60, folded into -01:00"},
	{"2026-07-31T01:12:34+24:00", "offset hour 24"},
	{"2026-07-31T01:12:34-24:00", "offset hour -24"},
	{"2026-07-31T01:12:34,5Z", "comma fractional separator"},
}

// TestGg_StrictRFC3339 pins this package's copy of the strict gate directly
// (#218). internal/google must not import internal/mora (mora imports google), so
// the seam that guards Mora's browse timestamps is duplicated here; these cases
// mirror internal/mora/recency_test.go so the two copies cannot drift apart
// silently.
//
// The oracle is an explicit transcription of the RFC 3339 §5.6 ABNF, never
// time.Parse — time.Parse is the thing being guarded against.
func TestGg_StrictRFC3339(t *testing.T) {
	grammar := regexp.MustCompile(
		`^\d{4}-(0[1-9]|1[0-2])-(0[1-9]|[12]\d|3[01])` +
			`T([01]\d|2[0-3]):[0-5]\d:[0-5]\d(\.\d+)?` +
			`(Z|[+-]([01]\d|2[0-3]):[0-5]\d)$`)

	valid := []string{
		"2026-07-31T01:12:34Z",
		"2026-07-31T00:00:00Z",
		"2026-07-31T23:59:59Z",
		"2026-07-31T01:12:34.5Z",
		"2026-07-31T01:12:34.000000001Z",
		"2026-07-31T01:12:34-04:00",
		"2026-07-31T01:12:34+05:30",
		"2026-07-31T01:12:34+23:59",
		"2026-07-31T01:12:34-00:00",
		"2028-02-29T12:00:00Z",
	}
	for _, s := range valid {
		if !grammar.MatchString(s) {
			t.Fatalf("the ABNF oracle rejected %q — the oracle is wrong", s)
		}
		if !strictRFC3339(s) {
			t.Errorf("strictRFC3339(%q) = false, want true", s)
		}
		if _, ok := rfc3339Instant(s); !ok {
			t.Errorf("rfc3339Instant(%q) rejected a legal stamp", s)
		}
	}

	// The forms the pinned toolchain's time.Parse accepts and RFC 3339 does not.
	// Pinned as a set: if a future Go tightens up, this says so rather than letting
	// the gate quietly become dead code.
	for _, tc := range ggRFC3339GoTolerates {
		if _, err := time.Parse(time.RFC3339, tc.stamp); err != nil {
			t.Errorf("time.Parse now rejects %q (%s) — re-check this gate's doc comment", tc.stamp, tc.why)
		}
		if grammar.MatchString(tc.stamp) {
			t.Errorf("the ABNF oracle accepted %q (%s) — the oracle is wrong", tc.stamp, tc.why)
		}
		if strictRFC3339(tc.stamp) {
			t.Errorf("strictRFC3339(%q) = true (%s), want false", tc.stamp, tc.why)
		}
	}

	// Malformed shapes, judged against the ABNF transcription. "2026-02-30" is the
	// one the regexp cannot settle: syntactically perfect and simply not a date,
	// which is deliberately time.Parse's half of the job inside rfc3339Instant.
	for _, s := range []string{
		"", "Z", "2026", "2026-07-31T", "2026-07-31T01:12:3",
		"2026-07-31T01:12:34", "2026-07-31T01:12:34+", "2026-07-31T01:12:34+0",
		"2026-07-31T01:12:34.", "2026-07-31T01:12:34.Z", "2026-07-31T01:12:34.-04:00",
		"2026-07-31T01:12:34+0100", "2026-07-31T01:12:34+1:00", "2026-07-31T01:12:34+00:0",
		"2026-07-31T01:12:34z", "2026-07-31t01:12:34Z", "2026-07-31 01:12:34Z",
		"2026-07-31T01:12:34Z ", "2026-07-31T01:12:60Z", "2026-13-01T01:12:34Z",
		"2026-00-01T01:12:34Z", "2026-07-00T01:12:34Z", "2026-07-32T01:12:34Z",
		"2026-07-31T24:12:34Z", "2026-07-31T01:60:34Z", "2026-07-31T01:12:34+00:60",
		"20é6-07-31T01:12:34Z", "2026-07-31T01:12:34é", "yesterday",
	} {
		if grammar.MatchString(s) {
			t.Fatalf("the ABNF oracle accepted %q — the oracle is wrong", s)
		}
		if strictRFC3339(s) {
			t.Errorf("strictRFC3339(%q) = true, want false", s)
		}
		if _, ok := rfc3339Instant(s); ok {
			t.Errorf("rfc3339Instant(%q) = ok, want rejected", s)
		}
	}
	if !strictRFC3339("2026-02-30T01:12:34Z") {
		t.Error("2026-02-30T01:12:34Z is syntactically valid; the calendar check belongs to time.Parse, not the syntax gate")
	}
	if _, ok := rfc3339Instant("2026-02-30T01:12:34Z"); ok {
		t.Error("rfc3339Instant accepted a date that does not exist")
	}
}

func TestGg_ParseCalTime(t *testing.T) {
	if got := parseCalTime(nil); !got.IsZero() {
		t.Fatalf("nil => %v, want zero", got)
	}

	t.Run("valid DateTime offsets and fractions retain their instant", func(t *testing.T) {
		for _, tc := range []struct {
			stamp string
			want  time.Time
		}{
			{"2026-06-04T09:30:00Z", time.Date(2026, 6, 4, 9, 30, 0, 0, time.UTC)},
			{"2026-06-04T15:00:00+05:30", time.Date(2026, 6, 4, 9, 30, 0, 0, time.UTC)},
			{"2026-06-04T09:30:00.75Z", time.Date(2026, 6, 4, 9, 30, 0, 750000000, time.UTC)},
		} {
			got := parseCalTime(&calendar.EventDateTime{DateTime: tc.stamp})
			if !got.Equal(tc.want) {
				t.Errorf("parseCalTime(%q) = %v, want instant %v", tc.stamp, got, tc.want)
			}

			item := calEventToItem("primary", &calendar.Event{
				Id:    "valid-start",
				Start: &calendar.EventDateTime{DateTime: tc.stamp},
			})
			if !item.OccurredAt.Equal(tc.want) {
				t.Errorf("calEventToItem(%q).OccurredAt = %v, want %v", tc.stamp, item.OccurredAt, tc.want)
			}
			if gotMeta, _ := item.Meta["occurred_at"].(string); gotMeta != tc.want.UTC().Format(time.RFC3339) {
				t.Errorf("calEventToItem(%q) meta[occurred_at] = %q, want UTC normalization %q",
					tc.stamp, gotMeta, tc.want.UTC().Format(time.RFC3339))
			}
		}
	})

	t.Run("Go-permissive non-RFC3339 DateTime is omitted before normalization", func(t *testing.T) {
		for _, tc := range ggRFC3339GoTolerates {
			tc := tc
			t.Run(tc.why, func(t *testing.T) {
				start := &calendar.EventDateTime{DateTime: tc.stamp}
				if got := parseCalTime(start); !got.IsZero() {
					t.Fatalf("parseCalTime(%q) = %v, want zero", tc.stamp, got)
				}

				// A malformed provider start must not be laundered into the
				// well-formed-but-wrong occurred_at consumed as event_start.
				item := calEventToItem("primary", &calendar.Event{
					Id:     "bad-start",
					Status: "cancelled",
					Start:  start,
				})
				if !item.OccurredAt.IsZero() {
					t.Fatalf("calEventToItem(%q).OccurredAt = %v, want zero", tc.stamp, item.OccurredAt)
				}
				if got, present := item.Meta["occurred_at"]; present {
					t.Fatalf("calEventToItem(%q) meta[occurred_at] = %v, want key omitted", tc.stamp, got)
				}
				if !item.Deleted {
					t.Fatal("rejecting a malformed start lost the event tombstone")
				}
			})
		}
	})

	t.Run("invalid DateTime preserves established all-day Date fallback", func(t *testing.T) {
		start := &calendar.EventDateTime{DateTime: "not-a-time", Date: "2026-06-04"}
		got := parseCalTime(start)
		want := time.Date(2026, 6, 4, 0, 0, 0, 0, time.UTC)
		if !got.Equal(want) {
			t.Fatalf("Date fallback parse = %v, want %v", got, want)
		}
		item := calEventToItem("primary", &calendar.Event{Id: "all-day", Start: start})
		if !item.OccurredAt.Equal(want) {
			t.Fatalf("all-day OccurredAt = %v, want %v", item.OccurredAt, want)
		}
		if gotMeta, _ := item.Meta["occurred_at"].(string); gotMeta != "2026-06-04T00:00:00Z" {
			t.Fatalf("all-day meta[occurred_at] = %q, want midnight UTC", gotMeta)
		}
	})

	t.Run("invalid or empty Date is zero", func(t *testing.T) {
		if got := parseCalTime(&calendar.EventDateTime{Date: "13/40/2026"}); !got.IsZero() {
			t.Fatalf("invalid Date => %v, want zero", got)
		}
		if got := parseCalTime(&calendar.EventDateTime{}); !got.IsZero() {
			t.Fatalf("empty => %v, want zero", got)
		}
	})
}
