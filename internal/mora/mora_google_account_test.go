package mora

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pyranthus-hq/mora/internal/google"
)

// TestGoogleTokenPathPerAccount locks the multi-account token layout: the
// default (unlabeled) account keeps the legacy tokens/google.json — existing
// installs keep working untouched — and a labeled account (e.g. a second
// work mailbox) gets its own tokens/google-<label>.json so two Google
// identities never clobber each other's refresh tokens.
func TestGoogleTokenPathPerAccount(t *testing.T) {
	cfg := Config{ConfigDir: "/c"}
	if got := googleTokenPathFor(cfg, ""); got != filepath.Join("/c", "tokens", "google.json") {
		t.Fatalf("default account path = %q", got)
	}
	if got := googleTokenPathFor(cfg, "work"); got != filepath.Join("/c", "tokens", "google-work.json") {
		t.Fatalf("labeled account path = %q", got)
	}
	if googleTokenPath(cfg) != googleTokenPathFor(cfg, "") {
		t.Fatalf("googleTokenPath must stay the unlabeled alias")
	}
}

// TestEnsureGoogleSourcesAccount locks the per-account source registration:
// a labeled connect registers gmail-<label>/calendar-<label> rows carrying
// Account=<label> (disabled by default, D-11), WITHOUT touching the default
// gmail/calendar rows — so personal + business mailboxes coexist as separate
// sources with separate sync status, digest sections, and provenance.
func TestEnsureGoogleSourcesAccount(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if err := ensureGoogleSources(cfg, ""); err != nil {
		t.Fatalf("ensureGoogleSources default: %v", err)
	}
	if err := ensureGoogleSources(cfg, "work"); err != nil {
		t.Fatalf("ensureGoogleSources work: %v", err)
	}
	// Idempotent: re-running adds nothing.
	if err := ensureGoogleSources(cfg, "work"); err != nil {
		t.Fatalf("ensureGoogleSources work (2nd): %v", err)
	}
	sources, _ := loadSources(cfg)
	byName := map[string]Source{}
	for _, s := range sources {
		byName[s.Name] = s
	}
	for name, wantAccount := range map[string]string{
		"gmail": "", "calendar": "", "gmail-work": "work", "calendar-work": "work",
	} {
		s, ok := byName[name]
		if !ok {
			t.Fatalf("missing source %q; have %v", name, sources)
		}
		if s.Account != wantAccount {
			t.Fatalf("source %q Account = %q, want %q", name, s.Account, wantAccount)
		}
	}
	if len(sources) != 4 {
		t.Fatalf("expected exactly 4 google sources after idempotent re-run, got %d", len(sources))
	}
}

// TestMultiAccountInstanceKey locks the "provider:account" composite seam the
// connectors.go comment promised: a labeled account's memories key their own
// watermark bucket / digest section, the source-side twin agrees by
// construction, enumeration emits both instances, and the display label
// distinguishes the mailboxes (never two sections both named "Emails").
func TestMultiAccountInstanceKey(t *testing.T) {
	if k, ok := sourceInstanceKey(Memory{Provider: "gmail"}); !ok || k != "gmail" {
		t.Fatalf("default account key = %q", k)
	}
	if k, ok := sourceInstanceKey(Memory{Provider: "gmail", Account: "work"}); !ok || k != "gmail:work" {
		t.Fatalf("labeled account key = %q", k)
	}
	if k := instanceKeyForSource(Source{Type: "gmail", Account: "work"}); k != "gmail:work" {
		t.Fatalf("source-side key = %q", k)
	}
	rank, label := connectorDisplay("gmail:work")
	rankDefault, labelDefault := connectorDisplay("gmail")
	if rank != rankDefault {
		t.Fatalf("composite must inherit the provider rank: %d vs %d", rank, rankDefault)
	}
	if label == labelDefault || !strings.Contains(label, "work") {
		t.Fatalf("composite label must carry the account: %q", label)
	}

	withTempHome(t)
	run(t, "init")
	cfg, _ := loadConfig()
	if err := ensureGoogleSources(cfg, ""); err != nil {
		t.Fatal(err)
	}
	if err := ensureGoogleSources(cfg, "work"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"gmail", "gmail-work"} {
		if err := setSourceEnabled(cfg, name, true); err != nil {
			t.Fatal(err)
		}
	}
	keys, err := ingestingConnectors(cfg)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(keys, ",")
	if !strings.Contains(joined, "gmail:work") || !strings.Contains(joined, "gmail") {
		t.Fatalf("enumeration must carry both instances, got %v", keys)
	}
}

// TestIngestGoogleRoutesAccountToken locks the routing: ingest for an
// account-labeled source must load THAT account's token, and the
// not-connected error must name the account-scoped connect command so the
// user isn't told to re-auth the wrong mailbox.
func TestIngestGoogleRoutesAccountToken(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	// Real-looking (non-placeholder) creds so ResolveOAuthConfig passes and the
	// failure under test is the missing per-account token, not OAuth setup.
	creds := filepath.Join(t.TempDir(), "client.json")
	if err := os.WriteFile(creds, []byte(`{"installed":{"client_id":"x.apps.googleusercontent.com","client_secret":"y","auth_uri":"https://a","token_uri":"https://t"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MORA_GOOGLE_CREDENTIALS", creds)

	s := Source{Name: "gmail-work", Type: "gmail", Account: "work", Enabled: ptr(true)}
	_, err = ingestGoogle(cfg, s, google.KindGmailThread, nil)
	if err == nil {
		t.Fatalf("expected not-connected error for missing work token")
	}
	if !strings.Contains(err.Error(), "--account work") {
		t.Fatalf("error must point at the account-scoped connect, got: %v", err)
	}
}
