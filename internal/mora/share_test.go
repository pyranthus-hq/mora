package mora

import (
	"bytes"
	"context"
	"io"
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

func testRecipient(t *testing.T) string {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return id.Recipient().String()
}

func TestShareInitCreatesStagingManifestAndRegistry(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	fx := &fakeExec{out: map[string]string{}, errOn: map[string]error{}}
	var buf bytes.Buffer

	rec := testRecipient(t)
	err := shareInit(context.Background(), cfg,
		[]string{"acme", "--scope", "project:acme", "--recipient", rec, "--remote", "git@example.test:me/vault.git"},
		&buf, fx.run)
	if err != nil {
		t.Fatalf("shareInit: %v", err)
	}

	staging := shareStagingDir(cfg, "acme")
	if !fx.sawSubcommand("git", "init") {
		t.Fatal("git init not run in staging dir")
	}
	if !fx.sawSubcommand("git", "remote", "add", "origin", "git@example.test:me/vault.git") {
		t.Fatalf("remote not wired; calls: %v", fx.calls)
	}
	gi, err := os.ReadFile(filepath.Join(staging, ".gitignore"))
	if err != nil || !strings.Contains(string(gi), "*.md") || !strings.Contains(string(gi), "identity") {
		t.Fatalf("staging .gitignore missing plaintext/identity defense: %v\n%s", err, gi)
	}
	mb, err := os.ReadFile(filepath.Join(staging, "share.json"))
	if err != nil || !strings.Contains(string(mb), `"project:acme"`) {
		t.Fatalf("manifest missing/wrong: %v\n%s", err, mb)
	}
	sf, err := loadShares(cfg)
	if err != nil || len(sf.Publishes) != 1 || sf.Publishes[0].Name != "acme" ||
		len(sf.Publishes[0].Recipients) != 1 || sf.Publishes[0].Recipients[0] != rec {
		t.Fatalf("registry entry wrong: %+v err=%v", sf, err)
	}
	low := strings.ToLower(buf.String())
	if !strings.Contains(low, "private") || !strings.Contains(low, "cannot recall") {
		t.Fatalf("init output missing PRIVATE-remote + honest-revocation disclosure:\n%s", buf.String())
	}
}

func TestShareInitValidation(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	rec := testRecipient(t)
	cases := [][]string{
		{"Acme", "--scope", "project:acme", "--recipient", rec, "--remote", "u"},             // bad name
		{"acme", "--scope", "project:../x", "--recipient", rec, "--remote", "u"},             // bad scope
		{"acme", "--scope", "project:acme", "--remote", "u"},                                 // no recipient
		{"acme", "--scope", "project:acme", "--recipient", "junk", "--remote", "u"},          // bad recipient
		{"acme", "--scope", "project:acme", "--recipient", rec, "--remote", "u", "--github"}, // both destinations
		{"acme", "--scope", "project:acme", "--recipient", rec},                              // no destination
	}
	for _, args := range cases {
		fx := &fakeExec{out: map[string]string{}, errOn: map[string]error{}}
		var buf bytes.Buffer
		if err := shareInit(context.Background(), cfg, args, &buf, fx.run); err == nil {
			t.Errorf("shareInit(%v) accepted; want error", args)
		}
	}
}

func TestShareInitRefusesDuplicateName(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	rec := testRecipient(t)
	fx := &fakeExec{out: map[string]string{}, errOn: map[string]error{}}
	var buf bytes.Buffer
	args := []string{"acme", "--scope", "project:acme", "--recipient", rec, "--remote", "git@example.test:me/vault.git"}
	if err := shareInit(context.Background(), cfg, args, &buf, fx.run); err != nil {
		t.Fatal(err)
	}
	if err := shareInit(context.Background(), cfg, args, &buf, fx.run); err == nil || !strings.Contains(err.Error(), "already") {
		t.Fatalf("duplicate init = %v; want already-exists refusal", err)
	}
}

func TestCollectShareMemoriesRejectsUnsafeID(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	evil := filepath.Join(cfg.VaultDir, "memories", "project", "acme", "evil.md")
	if err := os.MkdirAll(filepath.Dir(evil), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(evil, []byte("---\nid: ../evil\nscope: project:acme\n---\n\nx\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := collectShareMemories(cfg, "project:acme")
	if err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("collect with traversal id = %v; want unsafe-id refusal", err)
	}
}

// setupPublish registers a publish grant directly and simulates `git init`'s
// side effect (fakeExec runs no real git, so .git must exist for the plain-dir
// repo check).
func setupPublish(t *testing.T, cfg Config, name, scope string, recipients ...string) {
	t.Helper()
	sf, err := loadShares(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sf.Publishes = append(sf.Publishes, sharePublish{
		Name: name, Scope: scope, Recipients: recipients,
		Remote: "git@example.test:me/vault.git", CreatedAt: "2026-07-01T00:00:00Z",
	})
	if err := saveShares(cfg, sf); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(shareStagingDir(cfg, name), ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
}

func decryptShare(t *testing.T, id *age.X25519Identity, path string) []byte {
	t.Helper()
	ct, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	r, err := age.Decrypt(bytes.NewReader(ct), id)
	if err != nil {
		t.Fatalf("decrypt %s: %v", path, err)
	}
	pt, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return pt
}

func TestSharePushEncryptsScopeExactlyAndPushes(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	want1 := seedAuthored(t, "project:acme", "Acme decision", "we chose sqlite for the index")
	want2 := seedAuthored(t, "project:acme", "Acme deadline", "ship friday")
	seedAuthored(t, "personal", "Private", "never leaves")
	setupPublish(t, cfg, "acme", "project:acme", id.Recipient().String())

	fx := &fakeExec{out: map[string]string{"git status": " M memories/x"}, errOn: map[string]error{}, hasOrigin: true}
	var buf bytes.Buffer
	if err := sharePush(context.Background(), cfg, []string{"acme", "--yes"}, &buf, strings.NewReader(""), fx.run); err != nil {
		t.Fatalf("sharePush: %v\n%s", err, buf.String())
	}

	memDir := filepath.Join(shareStagingDir(cfg, "acme"), "memories")
	ents, err := os.ReadDir(memDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 2 {
		t.Fatalf("staging holds %d files; want exactly 2 (scope only)", len(ents))
	}
	for _, wantID := range []string{want1, want2} {
		pt := decryptShare(t, id, filepath.Join(memDir, wantID+".md.age"))
		orig, err := os.ReadFile(filepath.Join(cfg.VaultDir, "memories", "project", "acme", wantID+".md"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(pt, orig) {
			t.Fatalf("decrypted %s differs from vault original", wantID)
		}
	}
	if !fx.sawSubcommand("git", "add", "-A") || !fx.sawSubcommand("git", "push", "origin", "HEAD") {
		t.Fatalf("expected add+push; calls: %v", fx.calls)
	}
	for _, c := range fx.calls {
		for _, a := range c {
			if a == "--force" || a == "-f" || a == "--force-with-lease" {
				t.Fatalf("forced push detected: %v", c)
			}
		}
	}
	// Preview lists the exact files that left.
	if !strings.Contains(buf.String(), want1) || !strings.Contains(buf.String(), want2) {
		t.Fatalf("push output does not list published memory ids:\n%s", buf.String())
	}
	if _, err := os.Stat(sharePushStatePath(cfg, "acme")); err != nil {
		t.Fatalf("push state not recorded: %v", err)
	}
}

func TestSharePushRefusesNonInteractiveWithoutYes(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	seedAuthored(t, "project:acme", "Acme decision", "content")
	setupPublish(t, cfg, "acme", "project:acme", testRecipient(t))

	fx := &fakeExec{out: map[string]string{}, errOn: map[string]error{}, hasOrigin: true}
	var buf bytes.Buffer
	err := sharePush(context.Background(), cfg, []string{"acme"}, &buf, strings.NewReader(""), fx.run)
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("non-interactive push without --yes = %v; want refusal pointing at --yes", err)
	}
	if fx.sawSubcommand("git", "push") {
		t.Fatal("push ran despite refused confirmation")
	}
	if _, err := os.Stat(filepath.Join(shareStagingDir(cfg, "acme"), "memories")); !os.IsNotExist(err) {
		t.Fatal("staging mutated before confirmation")
	}
}

func TestSharePushRefusesWithoutRecipients(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	seedAuthored(t, "project:acme", "Acme decision", "content")
	setupPublish(t, cfg, "acme", "project:acme") // hand-edited registry: no keys

	fx := &fakeExec{out: map[string]string{}, errOn: map[string]error{}, hasOrigin: true}
	var buf bytes.Buffer
	err := sharePush(context.Background(), cfg, []string{"acme", "--yes"}, &buf, strings.NewReader(""), fx.run)
	if err == nil || !strings.Contains(err.Error(), "encrypt") {
		t.Fatalf("push without recipients = %v; want encryption refusal", err)
	}
	if fx.sawSubcommand("git", "push") {
		t.Fatal("push ran without encryption keys")
	}
}

func TestSharePushHardStopsOnTrackedPlaintext(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	seedAuthored(t, "project:acme", "Acme decision", "content")
	setupPublish(t, cfg, "acme", "project:acme", testRecipient(t))

	fx := &fakeExec{out: map[string]string{"git ls-files": "leak.md\n"}, errOn: map[string]error{}, hasOrigin: true}
	var buf bytes.Buffer
	err := sharePush(context.Background(), cfg, []string{"acme", "--yes"}, &buf, strings.NewReader(""), fx.run)
	if err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("tracked plaintext = %v; want hard stop", err)
	}
	if fx.sawSubcommand("git", "commit") || fx.sawSubcommand("git", "push") {
		t.Fatalf("commit/push ran past the hard stop; calls: %v", fx.calls)
	}
}

func TestSharePushVerifiesOriginMatchesRegistry(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	seedAuthored(t, "project:acme", "Acme decision", "content")
	setupPublish(t, cfg, "acme", "project:acme", testRecipient(t))
	// Registry says one remote; the staging repo's origin says another.
	sf, _ := loadShares(cfg)
	sf.Publishes[0].Remote = "git@example.test:someone-else/other.git"
	if err := saveShares(cfg, sf); err != nil {
		t.Fatal(err)
	}

	fx := &fakeExec{out: map[string]string{}, errOn: map[string]error{}, hasOrigin: true}
	var buf bytes.Buffer
	err := sharePush(context.Background(), cfg, []string{"acme", "--yes"}, &buf, strings.NewReader(""), fx.run)
	if err == nil || !strings.Contains(err.Error(), "origin") {
		t.Fatalf("origin mismatch = %v; want refusal naming origin", err)
	}
	if fx.sawSubcommand("git", "push") {
		t.Fatal("pushed to a mismatched origin")
	}
}

func TestSharePushRemovesStaleStagedFiles(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	seedAuthored(t, "project:acme", "Keep", "kept content")
	setupPublish(t, cfg, "acme", "project:acme", id.Recipient().String())
	stale := filepath.Join(shareStagingDir(cfg, "acme"), "memories", "mem_20250101_000000_00000000.md.age")
	if err := os.MkdirAll(filepath.Dir(stale), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("old ciphertext"), 0o644); err != nil {
		t.Fatal(err)
	}

	fx := &fakeExec{out: map[string]string{"git status": " D memories/x"}, errOn: map[string]error{}, hasOrigin: true}
	var buf bytes.Buffer
	if err := sharePush(context.Background(), cfg, []string{"acme", "--yes"}, &buf, strings.NewReader(""), fx.run); err != nil {
		t.Fatalf("sharePush: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale staged file not removed — unshared memories must leave the repo")
	}
	if !strings.Contains(buf.String(), "mem_20250101_000000_00000000") {
		t.Fatalf("removal not shown in preview:\n%s", buf.String())
	}
}

func TestSharePushSecondRunNoChangesButStillPushes(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	seedAuthored(t, "project:acme", "Keep", "kept content")
	setupPublish(t, cfg, "acme", "project:acme", id.Recipient().String())

	fx := &fakeExec{out: map[string]string{"git status": " M x"}, errOn: map[string]error{}, hasOrigin: true}
	var buf bytes.Buffer
	if err := sharePush(context.Background(), cfg, []string{"acme", "--yes"}, &buf, strings.NewReader(""), fx.run); err != nil {
		t.Fatal(err)
	}
	memDir := filepath.Join(shareStagingDir(cfg, "acme"), "memories")
	ents, _ := os.ReadDir(memDir)
	before, err := os.ReadFile(filepath.Join(memDir, ents[0].Name()))
	if err != nil {
		t.Fatal(err)
	}

	fx2 := &fakeExec{out: map[string]string{"git status": ""}, errOn: map[string]error{}, hasOrigin: true}
	var buf2 bytes.Buffer
	if err := sharePush(context.Background(), cfg, []string{"acme", "--yes"}, &buf2, strings.NewReader(""), fx2.run); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(memDir, ents[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("unchanged memory was re-encrypted on second push (age churn)")
	}
	if !strings.Contains(buf2.String(), "no changes") {
		t.Fatalf("second push did not report no changes:\n%s", buf2.String())
	}
	if !fx2.sawSubcommand("git", "push", "origin", "HEAD") {
		t.Fatal("second push skipped the git push — remote could stay behind")
	}
}

func TestSharePushRecipientChangeReencryptsAll(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	id1, _ := age.GenerateX25519Identity()
	id2, _ := age.GenerateX25519Identity()
	seedAuthored(t, "project:acme", "Keep", "kept content")
	setupPublish(t, cfg, "acme", "project:acme", id1.Recipient().String())

	fx := &fakeExec{out: map[string]string{"git status": " M x"}, errOn: map[string]error{}, hasOrigin: true}
	var buf bytes.Buffer
	if err := sharePush(context.Background(), cfg, []string{"acme", "--yes"}, &buf, strings.NewReader(""), fx.run); err != nil {
		t.Fatal(err)
	}
	memDir := filepath.Join(shareStagingDir(cfg, "acme"), "memories")
	ents, _ := os.ReadDir(memDir)
	stagedPath := filepath.Join(memDir, ents[0].Name())

	sf, _ := loadShares(cfg)
	sf.Publishes[0].Recipients = append(sf.Publishes[0].Recipients, id2.Recipient().String())
	if err := saveShares(cfg, sf); err != nil {
		t.Fatal(err)
	}
	var buf2 bytes.Buffer
	fx2 := &fakeExec{out: map[string]string{"git status": " M x"}, errOn: map[string]error{}, hasOrigin: true}
	if err := sharePush(context.Background(), cfg, []string{"acme", "--yes"}, &buf2, strings.NewReader(""), fx2.run); err != nil {
		t.Fatal(err)
	}
	// The new recipient must be able to decrypt the re-encrypted file.
	pt := decryptShare(t, id2, stagedPath)
	if !strings.Contains(string(pt), "kept content") {
		t.Fatal("new recipient cannot decrypt after recipient change")
	}
}

func TestSharePreviewShowsExactContentWithoutGit(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	want := seedAuthored(t, "project:acme", "Acme decision", "the exact body that will leave")
	setupPublish(t, cfg, "acme", "project:acme", testRecipient(t))

	var buf bytes.Buffer
	if err := sharePreview(cfg, []string{"acme"}, &buf); err != nil {
		t.Fatalf("sharePreview: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, want) || !strings.Contains(out, "the exact body that will leave") {
		t.Fatalf("preview missing id or full content:\n%s", out)
	}
}
