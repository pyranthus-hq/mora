package loopbackhttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testServer(dispatch DispatchFunc) *Server {
	if dispatch == nil {
		dispatch = func(context.Context, string, map[string]any) (any, error) { return map[string]any{"ok": true}, nil }
	}
	return New(Options{Token: "tok", Port: 7777, Version: "1.2.3", AllowCall: func(n string) bool { return n == "think" }, Dispatch: dispatch, Health: func() Health { return Health{OK: true, State: "healthy"} }})
}
func request(t *testing.T, s *Server, method, path, body, auth, host string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Host = host
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}
func TestRoutesAreSingleRegistry(t *testing.T) {
	got := testServer(nil).Routes()
	want := []Route{{"GET", "/healthz"}, {"GET", "/{$}"}, {"POST", "/call"}, {"POST", "/think"}, {"POST", "/search"}, {"POST", "/write"}, {"POST", "/meeting-prep"}, {"GET", "/entity/{name}"}, {"GET", "/brief"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("routes=%#v", got)
	}
}
func TestHostAuthAndNoCORS(t *testing.T) {
	s := testServer(nil)
	for _, host := range []string{"127.0.0.1:7777", "localhost:7777", "[::1]:7777"} {
		w := request(t, s, "GET", "/healthz", "", "", host)
		if w.Code != 200 {
			t.Errorf("host %s: %d", host, w.Code)
		}
	}
	for _, host := range []string{"evil.test:7777", "127.0.0.1", "localhost:9"} {
		w := request(t, s, "GET", "/healthz", "", "", host)
		if w.Code != 403 {
			t.Errorf("bad host %s: %d", host, w.Code)
		}
	}
	for _, path := range []string{"/", "/healthz"} {
		if w := request(t, s, "GET", path, "", "", "127.0.0.1:7777"); w.Code != 200 {
			t.Errorf("public %s=%d", path, w.Code)
		}
	}
	for _, auth := range []string{"", "Bearer bad"} {
		w := request(t, s, "POST", "/think", "{}", auth, "127.0.0.1:7777")
		if w.Code != 401 {
			t.Errorf("auth %q=%d", auth, w.Code)
		}
	}
	for _, auth := range []string{"Bearer tok", "tok"} {
		w := request(t, s, "POST", "/think", "{}", auth, "127.0.0.1:7777")
		if w.Code != 200 {
			t.Errorf("characterized auth %q=%d", auth, w.Code)
		}
		if w.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatal("CORS header present")
		}
	}
}
func TestCallBranches(t *testing.T) {
	calls := 0
	s := testServer(func(_ context.Context, n string, a map[string]any) (any, error) {
		calls++
		if a["fail"] == true {
			return nil, errors.New("boom")
		}
		return map[string]any{"name": n}, nil
	})
	cases := []struct {
		body string
		code int
	}{{"{", 400}, {`{"name":"delete_memory"}`, 403}, {`{"name":"think","arguments":{}}`, 200}, {`{"name":"think","arguments":{"fail":true}}`, 400}}
	for _, tc := range cases {
		w := request(t, s, "POST", "/call", tc.body, "Bearer tok", "127.0.0.1:7777")
		if w.Code != tc.code {
			t.Errorf("body %s code=%d body=%s", tc.body, w.Code, w.Body)
		}
	}
	if calls != 2 {
		t.Fatalf("calls=%d", calls)
	}
}
func TestConvenienceTranslations(t *testing.T) {
	var gotName string
	var got map[string]any
	s := testServer(func(_ context.Context, n string, a map[string]any) (any, error) {
		gotName = n
		got = a
		return map[string]any{}, nil
	})
	cases := []struct {
		method, path, body, name string
		want                     map[string]any
	}{{"POST", "/think", `{"q":"Q","query":"ignored","limit":0}`, "think", map[string]any{"query": "Q", "limit": float64(0)}}, {"POST", "/search", `{"query":"Q"}`, "search_memory", map[string]any{"query": "Q"}}, {"POST", "/write", `{"title":"T","text":"X"}`, "write_memory", map[string]any{"title": "T", "text": "X"}}, {"POST", "/meeting-prep", `{"event_id":"E","max_tokens":12}`, "meeting_prep", map[string]any{"event_id": "E", "max_tokens": float64(12)}}, {"GET", "/entity/Ada", "", "get_entity", map[string]any{"name": "Ada"}}, {"GET", "/brief?entity=Ada&envelope=1&max_tokens=9&since_days=no", "", "brief", map[string]any{"entity": "Ada", "envelope": true, "max_tokens": float64(9)}}}
	for _, tc := range cases {
		w := request(t, s, tc.method, tc.path, tc.body, "Bearer tok", "127.0.0.1:7777")
		if w.Code != 200 {
			t.Fatalf("%s=%d", tc.path, w.Code)
		}
		if gotName != tc.name || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s got %s %#v want %s %#v", tc.path, gotName, got, tc.name, tc.want)
		}
	}
}
func TestHealthAndLanding(t *testing.T) {
	s := New(Options{Token: "tok", Port: 7777, Version: "v", Health: func() Health { return Health{State: "unhealthy", Err: errors.New("cfg")} }})
	w := request(t, s, "GET", "/healthz", "", "", "127.0.0.1:7777")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"error": "cfg"`) {
		t.Fatalf("health=%d %s", w.Code, w.Body)
	}
	w = request(t, s, "GET", "/", "", "", "127.0.0.1:7777")
	if !strings.Contains(w.Body.String(), `window.__MORA_TOKEN="tok"`) || w.Header().Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("landing=%s", w.Body)
	}
}
func TestTokenReuseGenerationAndMalformedReplacement(t *testing.T) {
	for _, seed := range []string{"", `{"token":"stable"}`, `{"token":""}`, `bad`} {
		t.Run(fmt.Sprintf("seed-%d", len(seed)), func(t *testing.T) {
			d := t.TempDir()
			p := filepath.Join(d, "http.json")
			if seed != "" {
				if err := os.WriteFile(p, []byte(seed), 0600); err != nil {
					t.Fatal(err)
				}
			}
			a, err := LoadOrCreateToken(d)
			if err != nil {
				t.Fatal(err)
			}
			b, err := LoadOrCreateToken(d)
			if err != nil {
				t.Fatal(err)
			}
			if a != b || len(a) == 0 {
				t.Fatalf("tokens %q %q", a, b)
			}
			if seed == `{"token":"stable"}` && a != "stable" {
				t.Fatalf("replaced stable")
			}
			if info, err := os.Stat(p); err != nil || info.Mode().Perm() != 0600 {
				t.Fatalf("mode %v %v", info, err)
			}
		})
	}
}
func TestDispatchSerialized(t *testing.T) {
	var active, max atomic.Int32
	release := make(chan struct{})
	entered := make(chan struct{}, 2)
	s := testServer(func(context.Context, string, map[string]any) (any, error) {
		n := active.Add(1)
		for {
			m := max.Load()
			if n <= m || max.CompareAndSwap(m, n) {
				break
			}
		}
		entered <- struct{}{}
		<-release
		active.Add(-1)
		return map[string]any{}, nil
	})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); request(t, s, "POST", "/think", "{}", "Bearer tok", "127.0.0.1:7777") }()
	}
	<-entered
	select {
	case <-entered:
		t.Fatal("dispatches overlapped")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	wg.Wait()
	if max.Load() != 1 {
		t.Fatalf("max=%d", max.Load())
	}
}
func TestServePortGuardAndCancellation(t *testing.T) {
	for _, p := range []int{0, -1, 65536} {
		if err := New(Options{Port: p}).Serve(context.Background(), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "invalid --port") {
			t.Errorf("port %d err=%v", p, err)
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var out bytes.Buffer
	s := New(Options{Token: "tok", Port: p, Version: "v", Dispatch: func(context.Context, string, map[string]any) (any, error) { return nil, nil }, AllowCall: func(string) bool { return true }, Health: func() Health { return Health{OK: true, State: "healthy"} }})
	go func() { done <- s.Serve(ctx, &out) }()
	deadline := time.Now().Add(time.Second)
	for {
		conn, dialErr := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", p), 10*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not listen: %v", dialErr)
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown timeout")
	}
}
func TestOversizeCallBodyRejected(t *testing.T) {
	s := testServer(nil)
	body := `{"name":"think","arguments":{"x":"` + strings.Repeat("a", 1<<20) + `"}}`
	w := request(t, s, "POST", "/call", body, "Bearer tok", "127.0.0.1:7777")
	if w.Code != 400 {
		t.Fatalf("code=%d", w.Code)
	}
}
func TestJSONResponseFormat(t *testing.T) {
	w := request(t, testServer(nil), "POST", "/think", "{}", "Bearer tok", "127.0.0.1:7777")
	if w.Header().Get("Content-Type") != "application/json; charset=utf-8" || !strings.HasSuffix(w.Body.String(), "\n") || !json.Valid(w.Body.Bytes()) {
		t.Fatalf("headers/body=%v %q", w.Header(), w.Body.String())
	}
}
