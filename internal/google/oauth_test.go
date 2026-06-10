package google

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

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
	if info.Mode().Perm() != 0o600 {
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
