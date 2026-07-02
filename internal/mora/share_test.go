package mora

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"filippo.io/age"
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

func TestShareKeygenCreatesIdentityAndRefusesOverwrite(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	out := run(t, "share", "keygen")
	if !strings.Contains(out, "age1") {
		t.Fatalf("keygen did not print a public key:\n%s", out)
	}
	ids, err := loadShareIdentities(cfg)
	if err != nil || len(ids) != 1 {
		t.Fatalf("loadShareIdentities = %v, %v; want one identity", ids, err)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(shareIdentityPath(cfg))
		if err != nil {
			t.Fatalf("stat identity: %v", err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("identity mode = %v; want 0600", fi.Mode().Perm())
		}
	}

	var buf bytes.Buffer
	err = Run(context.Background(), []string{"share", "keygen"}, &buf, &buf, strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("second keygen = %v; want refuse-to-overwrite error", err)
	}
}

func TestLoadShareIdentitiesMissingIsActionable(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	_, err := loadShareIdentities(cfg)
	if err == nil || !strings.Contains(err.Error(), "mora share keygen") {
		t.Fatalf("missing identity error = %v; want pointer to `mora share keygen`", err)
	}
}

func TestParseShareRecipients(t *testing.T) {
	if _, err := parseShareRecipients(nil); err == nil {
		t.Fatal("empty recipient list accepted; sharing must refuse without encryption keys")
	}
	if _, err := parseShareRecipients([]string{"garbage"}); err == nil {
		t.Fatal("garbage recipient accepted")
	}
	if _, err := parseShareRecipients([]string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPuh4bTTh2mkV2kbwCuvsWG6SGuvbUf4DvOJzKZ9d9d9"}); err == nil {
		t.Fatal("ssh recipient accepted; v1 is X25519-only")
	}
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseShareRecipients([]string{" " + id.Recipient().String() + " "})
	if err != nil || len(got) != 1 {
		t.Fatalf("valid X25519 recipient rejected: %v", err)
	}
}

// seedAuthored writes an authored memory via the real CLI path and returns its id.
func seedAuthored(t *testing.T, scope, title, text string) string {
	t.Helper()
	out := run(t, "write", "--scope", scope, "--title", title, "--text", text)
	for _, f := range strings.Fields(out) {
		if strings.HasPrefix(f, "mem_") {
			return f
		}
	}
	t.Fatalf("no mem_ id in write output:\n%s", out)
	return ""
}

func TestCollectShareMemoriesSelectsAuthoredScopeOnly(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	want1 := seedAuthored(t, "project:acme", "Acme decision", "we chose sqlite")
	want2 := seedAuthored(t, "project:acme", "Acme deadline", "ship friday")
	seedAuthored(t, "personal", "Private note", "not for export")
	seedAuthored(t, "project:other", "Other project", "not for export either")

	// A tombstoned authored memory in scope must be excluded.
	tomb := Memory{ID: "mem_20260101_000000_deadbeef", Scope: "project:acme", Type: "insight",
		Title: "Withdrawn", CreatedAt: "2026-01-01T00:00:00Z", DeletedAt: "2026-06-01T00:00:00Z", Text: "gone"}
	if err := writeMemory(cfg, tomb); err != nil {
		t.Fatal(err)
	}

	// A connector memory under sources/ with the same scope must be structurally
	// invisible (v1 shares authored notes only).
	conn := Memory{ID: "gmail_thread/abc", Scope: "project:acme", Type: "email", Title: "Thread",
		Provider: "gmail", ProviderID: "abc", CreatedAt: "2026-01-01T00:00:00Z", Text: "connector evidence"}
	connPath := filepath.Join(cfg.VaultDir, "sources", "gmail", "gmail_thread_abc.md")
	if err := os.MkdirAll(filepath.Dir(connPath), 0o700); err != nil {
		t.Fatal(err)
	}
	connBody, err := renderMemory(conn)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(connPath, connBody, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := collectShareMemories(cfg, "project:acme")
	if err != nil {
		t.Fatalf("collectShareMemories: %v", err)
	}
	ids := make([]string, len(got))
	for i, m := range got {
		ids[i] = m.ID
	}
	if len(got) != 2 || !(ids[0] == want1 || ids[1] == want1) || !(ids[0] == want2 || ids[1] == want2) {
		t.Fatalf("collected %v; want exactly {%s, %s}", ids, want1, want2)
	}
	for i := 1; i < len(ids); i++ {
		if ids[i-1] >= ids[i] {
			t.Fatalf("ids not sorted deterministically: %v", ids)
		}
	}
}

func TestCollectShareMemoriesRefusesSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks not exercised on windows")
	}
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	seedAuthored(t, "project:acme", "Real", "real content")

	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("id: mem_x\nscope: project:acme\n---\n\nsmuggled\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(cfg.VaultDir, "memories", "project", "acme", "link.md")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	_, err := collectShareMemories(cfg, "project:acme")
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("collect with symlink = %v; want loud symlink refusal", err)
	}
}

func TestCollectShareMemoriesRejectsInvalidScope(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	for _, s := range []string{"project:../x", "..", "project:a/b", ""} {
		if _, err := collectShareMemories(cfg, s); err == nil {
			t.Fatalf("scope %q accepted; want rejection before any walk", s)
		}
	}
}

// If the user re-points data_dir INSIDE the vault, a decrypted subscription
// corpus would land under the vault and leak into `mora backup` / vault
// git-sync. Every share verb must refuse loudly instead (codex review P0).
func TestShareRefusesWhenShareRootInsideVault(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	f, err := os.OpenFile(filepath.Join(cfg.ConfigDir, "config.toml"), os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("data_dir = \"" + filepath.Join(cfg.VaultDir, "data") + "\"\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	var buf bytes.Buffer
	err = Run(context.Background(), []string{"share", "keygen"}, &buf, &buf, strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "inside the vault") {
		t.Fatalf("share with data_dir inside vault = %v; want refusal naming the vault", err)
	}
}
