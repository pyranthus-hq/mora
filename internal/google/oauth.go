package google

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"google.golang.org/api/calendar/v3"
	"google.golang.org/api/gmail/v1"
)

// embeddedClientJSON holds the OAuth client config baked into the binary.
// client.json is a committed NON-SECRET placeholder (so this embed compiles on a
// fresh clone); real creds are swapped in at release-build time or overridden at
// runtime via MORA_GOOGLE_CREDENTIALS (see ResolveOAuthConfig).
//
//go:embed client.json
var embeddedClientJSON []byte

// Scopes used by the v1 pilot (read-only, least-privilege; Drive deferred).
var Scopes = []string{gmail.GmailReadonlyScope, calendar.CalendarReadonlyScope}

// ResolveOAuthConfig prefers BYO creds (MORA_GOOGLE_CREDENTIALS) over the
// embedded shared "Mora" app creds.
func ResolveOAuthConfig(scopes []string) (*oauth2.Config, error) {
	raw := embeddedClientJSON
	if p := os.Getenv("MORA_GOOGLE_CREDENTIALS"); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		raw = b
	}
	return configFromInstalledJSON(raw, scopes)
}

// IsConfigured reports whether real (non-placeholder) Google OAuth creds are
// available, by reusing the SAME guard as ResolveOAuthConfig (configFromInstalledJSON:
// ClientID empty or DEV_PLACEHOLDER-prefixed ⇒ not configured). It opens NO browser
// and performs NO loopback — it is the detection seam the guided setup menu uses to
// skip Google without dead-ending when creds are a placeholder (UI-SPEC §C/E-7). It
// changes no OAuth mechanics; it only wraps the existing check.
func IsConfigured() bool {
	_, err := ResolveOAuthConfig(Scopes)
	return err == nil
}

type installedCreds struct {
	Installed struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
		AuthURI      string `json:"auth_uri"`
		TokenURI     string `json:"token_uri"`
	} `json:"installed"`
}

func configFromInstalledJSON(raw []byte, scopes []string) (*oauth2.Config, error) {
	var c installedCreds
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, err
	}
	// The repo ships a non-secret placeholder client.json so the //go:embed
	// compiles, but it cannot authorize against Google (Error 401:
	// invalid_client). Fail fast here with actionable guidance instead of
	// opening a doomed browser tab when no real BYO creds were supplied.
	if c.Installed.ClientID == "" || strings.HasPrefix(c.Installed.ClientID, "DEV_PLACEHOLDER") {
		// CROSS-PHASE TOUCH (UI-SPEC §C, copy-only): plain-language lead-in, no
		// user-facing jargon ("OAuth client ID" is kept only because it is the exact
		// Google Console menu label the user must click). Mechanics unchanged.
		return nil, fmt.Errorf(`Google sign-in needs a one-time setup on your own Google account (about 2 minutes). Mora ships without shared Google keys, so your data stays yours.

To set it up:
  1. Go to https://console.cloud.google.com/apis/credentials → Create credentials → OAuth client ID → Desktop app
  2. Download the client JSON
  3. Re-run with your file, e.g.:
       MORA_GOOGLE_CREDENTIALS=/path/to/client.json mora connect google

The filesystem and iMessage connectors need no setup and work without this`)
	}
	return &oauth2.Config{
		ClientID:     c.Installed.ClientID,
		ClientSecret: c.Installed.ClientSecret,
		Scopes:       scopes,
		Endpoint:     oauth2.Endpoint{AuthURL: c.Installed.AuthURI, TokenURL: c.Installed.TokenURI},
		// RedirectURL is set per-run to the loopback addr in StartLoopbackAuth (a later task).
	}, nil
}

func SaveToken(path string, tok *oauth2.Token) error {
	return saveTokenAt(path, tok, time.Now())
}

// saveTokenAt is SaveToken with an injected clock so the auth-event timestamp is
// testable. On a successful write it records the auth in the token dir's
// append-only auth-history.jsonl (so `mora doctor` can show "last authed").
// Recording is best-effort: the token write is what matters, so a RecordAuth
// failure does NOT fail the save.
func saveTokenAt(path string, tok *oauth2.Token, now time.Time) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(tok, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	// Best-effort: the account is the token filename minus ".json"
	// (tokens/google.json -> "google", tokens/google-work.json -> "google-work").
	account := strings.TrimSuffix(filepath.Base(path), ".json")
	_ = RecordAuth(filepath.Dir(path), account, now)
	return nil
}

func LoadToken(path string) (*oauth2.Token, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var tok oauth2.Token
	if err := json.Unmarshal(b, &tok); err != nil {
		return nil, err
	}
	return &tok, nil
}

// RevokeToken best-effort revokes a token at Google's revocation endpoint.
func RevokeToken(ctx context.Context, tok *oauth2.Token) error {
	req, err := http.NewRequestWithContext(ctx, "POST",
		"https://oauth2.googleapis.com/revoke?token="+tok.RefreshToken, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// IsWSL reports whether we're running under WSL (no auto-open browser).
func IsWSL() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	b, err := os.ReadFile("/proc/version")
	if err != nil {
		return false
	}
	return isWSLProcVersion(string(b))
}

func isWSLProcVersion(s string) bool {
	return strings.Contains(strings.ToLower(s), "microsoft")
}

// AuthResult carries the obtained token plus whether a refresh token was issued.
type AuthResult struct {
	Token *oauth2.Token
}

// StartLoopbackAuth runs the full consent flow on 127.0.0.1:<random port>.
// On WSL or when the browser cannot open, it prints the URL for manual paste.
func StartLoopbackAuth(ctx context.Context, cfg *oauth2.Config, out io.Writer) (*oauth2.Token, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("loopback listen: %w", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	cfg.RedirectURL = fmt.Sprintf("http://127.0.0.1:%d/callback", port)

	state := ContentHash(time.Now().UTC().String(), cfg.RedirectURL)
	authURL := cfg.AuthCodeURL(state,
		oauth2.AccessTypeOffline,                    // -> refresh token
		oauth2.SetAuthURLParam("prompt", "consent")) // re-consent still returns refresh token

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	mux := http.NewServeMux()
	srv := &http.Server{Handler: mux}
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			errCh <- fmt.Errorf("oauth state mismatch")
			return
		}
		if e := r.URL.Query().Get("error"); e != "" {
			http.Error(w, "auth error: "+e, http.StatusBadRequest)
			errCh <- fmt.Errorf("oauth error: %s", e)
			return
		}
		fmt.Fprintln(w, "Mora connected. You can close this tab.")
		codeCh <- r.URL.Query().Get("code")
	})
	go srv.Serve(ln)
	defer srv.Shutdown(context.Background())

	// Lead with what the user should EXPECT (a browser tab opening), keep the
	// raw URL as the fallback path, and say the wait resumes on its own — a
	// silent block after a wall of URL read as a dead command.
	if !IsWSL() {
		fmt.Fprintln(out, "\nOpening your browser for Google sign-in…")
		fmt.Fprintf(out, "If no tab appears, open this link yourself:\n  %s\n", authURL)
		_ = openBrowser(authURL) // best effort; URL printed as the fallback
	} else {
		fmt.Fprintln(out, "\nWSL detected — paste this link into your Windows browser to sign in:")
		fmt.Fprintf(out, "  %s\n", authURL)
	}
	fmt.Fprintln(out, "Waiting for you to finish in the browser… (resumes automatically; Ctrl-C to cancel)")

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case err := <-errCh:
		return nil, err
	case <-time.After(5 * time.Minute):
		return nil, fmt.Errorf("timed out waiting for authorization")
	case code := <-codeCh:
		tok, err := cfg.Exchange(ctx, code)
		if err != nil {
			return nil, fmt.Errorf("token exchange: %w", err)
		}
		if tok.RefreshToken == "" {
			return nil, fmt.Errorf("no refresh token returned; re-run with --reauth")
		}
		return tok, nil
	}
}

func openBrowser(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	case "linux":
		return exec.Command("xdg-open", url).Start()
	default:
		return fmt.Errorf("unsupported platform for auto-open")
	}
}
