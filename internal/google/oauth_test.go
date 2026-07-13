package google

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func TestResolveCredentialsPrefersEnv(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "byo.json")
	if err := os.WriteFile(p, []byte(`{"installed":{"client_id":"byo.apps.googleusercontent.com","client_secret":"s","auth_uri":"https://a","token_uri":"https://t"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MORA_GOOGLE_CREDENTIALS", p)

	scopes := []string{"scope"}
	cfg, err := ResolveOAuthConfig(scopes)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClientID != "byo.apps.googleusercontent.com" {
		t.Fatalf("env creds not used: %s", cfg.ClientID)
	}
	if !reflect.DeepEqual(cfg.Scopes, scopes) {
		t.Fatalf("scopes = %v, want %v", cfg.Scopes, scopes)
	}
}

// TestResolveCredentialsRejectsPlaceholder asserts the embedded NON-SECRET
// placeholder client.json cannot be used to authorize: ResolveOAuthConfig must
// fail fast with actionable guidance rather than building a config that yields
// Google's opaque "Error 401: invalid_client" in the browser.
func TestResolveCredentialsRejectsPlaceholder(t *testing.T) {
	// Empty env var forces the embedded placeholder path (p != "" is false).
	t.Setenv("MORA_GOOGLE_CREDENTIALS", "")

	cfg, err := ResolveOAuthConfig([]string{"scope"})
	if err == nil {
		t.Fatalf("placeholder client must error, got config %+v", cfg)
	}
	if !strings.Contains(err.Error(), "one-time setup") {
		t.Fatalf("error should explain the one-time BYO-creds setup in plain language, got: %v", err)
	}
}

func TestResolveCredentialsMissingEnvFileErrors(t *testing.T) {
	t.Setenv("MORA_GOOGLE_CREDENTIALS", filepath.Join(t.TempDir(), "missing.json"))

	cfg, err := ResolveOAuthConfig([]string{"scope"})
	if err == nil {
		t.Fatalf("missing env credentials should error, got config %+v", cfg)
	}
}

func TestOpenBrowserUsesPlatformCommand(t *testing.T) {
	origGOOS := browserGOOS
	origStart := startBrowserCommand
	t.Cleanup(func() {
		browserGOOS = origGOOS
		startBrowserCommand = origStart
	})

	tests := []struct {
		goos     string
		wantName string
		wantArgs []string
	}{
		{goos: "darwin", wantName: "open", wantArgs: []string{"https://example.test/auth"}},
		{goos: "linux", wantName: "xdg-open", wantArgs: []string{"https://example.test/auth"}},
		{goos: "windows", wantName: "rundll32", wantArgs: []string{"url.dll,FileProtocolHandler", "https://example.test/auth"}},
	}
	for _, tt := range tests {
		t.Run(tt.goos, func(t *testing.T) {
			browserGOOS = func() string { return tt.goos }
			var gotName string
			var gotArgs []string
			startBrowserCommand = func(name string, args ...string) error {
				gotName = name
				gotArgs = append([]string(nil), args...)
				return nil
			}

			if err := openBrowser("https://example.test/auth"); err != nil {
				t.Fatalf("openBrowser(%s): %v", tt.goos, err)
			}
			if gotName != tt.wantName || !reflect.DeepEqual(gotArgs, tt.wantArgs) {
				t.Fatalf("command = %s %#v, want %s %#v", gotName, gotArgs, tt.wantName, tt.wantArgs)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestRevokeToken(t *testing.T) {
	t.Run("success posts to revoke endpoint with the refresh token", func(t *testing.T) {
		var gotMethod, gotURL, gotContentType string
		old := http.DefaultClient
		http.DefaultClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
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
		http.DefaultClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
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

func TestOpenBrowserUnsupportedPlatform(t *testing.T) {
	origGOOS := browserGOOS
	t.Cleanup(func() { browserGOOS = origGOOS })

	browserGOOS = func() string { return "plan9" }
	if err := openBrowser("https://example.test/auth"); err == nil {
		t.Fatal("openBrowser on unsupported platform returned nil")
	}
}

func TestTokenStoreRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens", "google.json")
	tok := &oauth2.Token{AccessToken: "a", RefreshToken: "r"}
	if err := SaveToken(path, tok); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Windows has no Unix mode bits (Perm() is ACL-derived, reports 0666 for a
	// writable file), so the owner-only 0600 assertion only holds off Windows.
	// SaveToken still writes 0o600, which is security-relevant on Unix.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("token file must be 0600, got %v", info.Mode().Perm())
	}
	got, err := LoadToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.RefreshToken != "r" {
		t.Fatalf("refresh token not preserved: %+v", got)
	}
}

// TestSaveTokenAtRecordsAuthEvent asserts that saveTokenAt, with an injected
// clock, records an AuthEvent in the token file's directory with the account
// derived from the token filename and the injected timestamp.
func TestSaveTokenAtRecordsAuthEvent(t *testing.T) {
	dir := t.TempDir()
	tokenDir := filepath.Join(dir, "tokens")
	cases := []struct {
		name        string
		file        string // token filename
		wantAccount string
	}{
		{"default account", "google.json", "google"},
		{"labeled account", "google-work.json", "google-work"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(tokenDir, tc.file)
			now := time.Date(2026, 6, 15, 8, 30, 0, 0, time.UTC)
			tok := &oauth2.Token{AccessToken: "a", RefreshToken: "r"}
			if err := saveTokenAt(path, tok, now); err != nil {
				t.Fatalf("saveTokenAt: %v", err)
			}
			// The token itself must still be written (unchanged behavior).
			if _, err := LoadToken(path); err != nil {
				t.Fatalf("token not written: %v", err)
			}
			// The auth event lands in the token file's DIRECTORY.
			at, ok, err := LastAuth(tokenDir, tc.wantAccount)
			if err != nil {
				t.Fatalf("LastAuth: %v", err)
			}
			if !ok {
				t.Fatalf("saveTokenAt did not record an auth event for %q", tc.wantAccount)
			}
			if !at.Equal(now) {
				t.Fatalf("recorded auth time = %v, want injected %v", at, now)
			}
		})
	}
}

func TestLoadTokenMissingFileErrors(t *testing.T) {
	got, err := LoadToken(filepath.Join(t.TempDir(), "missing", "google.json"))
	if err == nil {
		t.Fatalf("missing token should error, got token %+v", got)
	}
}

func TestIsWSL(t *testing.T) {
	if isWSLProcVersion("Linux version 5.15.0-microsoft-standard-WSL2") != true {
		t.Fatal("should detect WSL")
	}
	if isWSLProcVersion("Linux version 5.15.90.1-MICROSOFT-standard-WSL2") != true {
		t.Fatal("should detect WSL case-insensitively")
	}
	if isWSLProcVersion("Linux version 6.1.0-generic") != false {
		t.Fatal("should not flag plain linux")
	}
}

func TestOpenBrowserRejectsInvalidURLs(t *testing.T) {
	origGOOS := browserGOOS
	origStart := startBrowserCommand
	t.Cleanup(func() {
		browserGOOS = origGOOS
		startBrowserCommand = origStart
	})

	browserGOOS = func() string { return "linux" }
	startBrowserCommand = func(name string, args ...string) error {
		return nil
	}

	tests := []struct {
		url     string
		wantErr string
	}{
		{url: "file:///etc/passwd", wantErr: "unsupported browser URL scheme: file"},
		{url: "javascript:alert(1)", wantErr: "unsupported browser URL scheme: javascript"},
		{url: ":/malformed", wantErr: "invalid URL for browser"},
		{url: "-http://example.com", wantErr: "invalid URL for browser"},
		{url: "http://example.com", wantErr: ""},
		{url: "https://example.com", wantErr: ""},
	}

	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			err := openBrowser(tt.url)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("expected no error, got %v", err)
				}
			} else {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
				}
			}
		})
	}
}
