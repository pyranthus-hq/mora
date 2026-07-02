package mora

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// `mora share --help` (and the bare family verb) must print usage and cause zero
// side effects — same help-guard contract as the other subcommand families.
func TestShareHelpPrintsUsage(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	out := run(t, "share", "--help")
	if !strings.Contains(strings.ToLower(out), "usage: mora share") {
		t.Fatalf("`share --help` did not print usage:\n%s", out)
	}
	cfg := mustConfig(t)
	if _, err := os.Stat(filepath.Join(cfg.ConfigDir, "shares.json")); !os.IsNotExist(err) {
		t.Fatalf("`share --help` touched shares.json (err=%v); want zero side effects", err)
	}
}

func TestShareBareAndUnknownSubverbError(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	for _, args := range [][]string{{"share"}, {"share", "bogus"}} {
		var out bytes.Buffer
		err := Run(context.Background(), args, &out, &out, strings.NewReader(""))
		if err == nil || !strings.Contains(err.Error(), "usage: mora share") {
			t.Fatalf("Run(%v) = %v; want usage error", args, err)
		}
	}
}

func TestValidShareName(t *testing.T) {
	valid := []string{"acme", "team-x", "a", "neil.work", "p_0"}
	invalid := []string{"", "Acme", "-lead", ".dot", "a/b", "a b", "a:b", "..", strings.Repeat("x", 65)}
	for _, s := range valid {
		if !validShareName(s) {
			t.Errorf("validShareName(%q) = false; want true", s)
		}
	}
	for _, s := range invalid {
		if validShareName(s) {
			t.Errorf("validShareName(%q) = true; want false", s)
		}
	}
}

// Share scopes are deliberately stricter than `mora write` scopes: they expand to
// directory names and travel between machines, so only the documented forms pass.
func TestValidShareScope(t *testing.T) {
	valid := []string{"personal", "global", "project:acme", "project:Acme-2.x", "project:a_b"}
	invalid := []string{"", "project:", "project:../x", "project:a/b", "personal/extra", "..", "project:.hidden", "team:acme"}
	for _, s := range valid {
		if !validShareScope(s) {
			t.Errorf("validShareScope(%q) = false; want true", s)
		}
	}
	for _, s := range invalid {
		if validShareScope(s) {
			t.Errorf("validShareScope(%q) = true; want false", s)
		}
	}
}

func TestSharesFileRoundTrip(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	// Missing file is an empty registry, not an error.
	sf, err := loadShares(cfg)
	if err != nil {
		t.Fatalf("loadShares (missing file): %v", err)
	}
	if len(sf.Publishes) != 0 || len(sf.Subscriptions) != 0 {
		t.Fatalf("loadShares (missing file) = %+v; want empty", sf)
	}

	sf.Publishes = append(sf.Publishes, sharePublish{
		Name: "acme", Scope: "project:acme",
		Recipients: []string{"age1zvxa9lhw45ta20u5rwvmtvz3lu034zzzc5eyeju53fknos0k5csq0er4rv"},
		Remote:     "git@example.com:me/acme-share.git", CreatedAt: "2026-07-01T00:00:00Z",
	})
	sf.Subscriptions = append(sf.Subscriptions, shareSubscription{
		Name: "neil", Remote: "git@example.com:neil/share.git", CreatedAt: "2026-07-01T00:00:00Z",
	})
	if err := saveShares(cfg, sf); err != nil {
		t.Fatalf("saveShares: %v", err)
	}

	got, err := loadShares(cfg)
	if err != nil {
		t.Fatalf("loadShares: %v", err)
	}
	if len(got.Publishes) != 1 || got.Publishes[0].Name != "acme" || got.Publishes[0].Scope != "project:acme" {
		t.Fatalf("publishes round-trip = %+v", got.Publishes)
	}
	if len(got.Subscriptions) != 1 || got.Subscriptions[0].Name != "neil" {
		t.Fatalf("subscriptions round-trip = %+v", got.Subscriptions)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(filepath.Join(cfg.ConfigDir, "shares.json"))
		if err != nil {
			t.Fatalf("stat shares.json: %v", err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("shares.json mode = %v; want 0600", fi.Mode().Perm())
		}
	}
}

// The share dirs live under DataDir — outside the vault — so the personal index
// walk, `mora backup`, and vault git-sync can never see them.
func TestSharePathsOutsideVault(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	for _, p := range []string{
		shareStagingDir(cfg, "x"),
		shareRepoDir(cfg, "x"),
		shareCorpusDir(cfg, "x"),
		shareIndexPath(cfg, "x"),
	} {
		rel, err := filepath.Rel(cfg.VaultDir, p)
		if err == nil && !strings.HasPrefix(rel, "..") {
			t.Fatalf("share path %q is inside the vault %q", p, cfg.VaultDir)
		}
	}
}
