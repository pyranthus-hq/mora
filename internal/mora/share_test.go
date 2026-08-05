package mora

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

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

func TestShareFingerprintPrintsStablePublicFingerprintOnly(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	first := run(t, "share", "fingerprint")
	priv, err := readSigningKey(shareSigningKeyPath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	pub := priv.Public().(ed25519.PublicKey)
	want := signPubFingerprint(pub)
	if !strings.Contains(first, want) {
		t.Fatalf("fingerprint output = %q; want %q", first, want)
	}
	if strings.Contains(first, string(priv)) || strings.Contains(first, hex.EncodeToString(priv)) {
		t.Fatal("fingerprint command printed private signing-key bytes")
	}
	if !strings.Contains(first, "safe to share") || !strings.Contains(first, "separate trusted channel") {
		t.Fatalf("fingerprint output lacks safe exchange guidance: %q", first)
	}
	if !strings.Contains(first, "--confirm-pin <fingerprint>") {
		t.Fatalf("fingerprint output lacks the subscriber command: %q", first)
	}

	second := run(t, "share", "fingerprint")
	if second != first {
		t.Fatalf("fingerprint changed across runs:\nfirst: %q\nsecond: %q", first, second)
	}
	info, err := os.Stat(shareSigningKeyPath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	// Windows reports synthesized POSIX mode bits (typically 0666). Its real
	// access control comes from the user's config directory ACL, so only assert
	// the requested 0600 mode where these bits are meaningful.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("signing key mode = %o, want 600", info.Mode().Perm())
	}
}

func TestShareFingerprintHelpHasNoSideEffects(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	out := run(t, "share", "fingerprint", "--help")
	if !strings.Contains(strings.ToLower(out), "usage: mora share") {
		t.Fatalf("fingerprint --help did not print usage:\n%s", out)
	}
	if _, err := os.Stat(shareSigningKeyPath(cfg)); !os.IsNotExist(err) {
		t.Fatal("fingerprint --help created a signing key")
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
	if len(got) != 2 || (ids[0] != want1 && ids[1] != want1) || (ids[0] != want2 && ids[1] != want2) {
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

// writeTestIdentity installs a subscriber age identity without going through
// keygen's stdout parsing.
func writeTestIdentity(t *testing.T, cfg Config) *age.X25519Identity {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(shareIdentityPath(cfg)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shareIdentityPath(cfg), []byte(id.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return id
}

// buildShareRepoFixture materializes what a publisher's push produces: the
// manifest plus age-encrypted memories, at the given repo dir. withGit adds a
// plain .git dir so subscribe/pull treat it as an existing clone.
func buildShareRepoFixture(t *testing.T, dir string, rec age.Recipient, mems []Memory, withGit bool) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "memories"), 0o700); err != nil {
		t.Fatal(err)
	}
	if withGit {
		if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	man := shareManifest{Schema: shareManifestSchema, Name: "acme", Scope: "project:acme",
		Owner: "Adit", CreatedAt: "2026-07-01T00:00:00Z", Client: "mora test"}
	mb, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "share.json"), append(mb, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, m := range mems {
		body, err := renderMemory(m)
		if err != nil {
			t.Fatal(err)
		}
		ct, err := encryptShareBytes([]age.Recipient{rec}, body)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "memories", m.ID+".md.age"), ct, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func fixtureMemory(id, title, text string) Memory {
	return Memory{ID: id, Scope: "project:acme", Type: "insight", Title: title,
		Source: "manual", CreatedAt: "2026-06-30T10:00:00Z", Text: text}
}

// mustGit runs a git command in dir, failing the test on error.
func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := realExec(context.Background(), dir, "git", args...)
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// realGitShareRemote builds a REAL committed git repo (share.json + encrypted
// memories) that subscribe can clone and the H1 pin can fetch from. Returns the
// repo path to pass as --remote.
func realGitShareRemote(t *testing.T, rec age.Recipient, mems []Memory) string {
	t.Helper()
	remote := t.TempDir()
	buildShareRepoFixture(t, remote, rec, mems, false)
	mustGit(t, remote, "init", "-q")
	mustGit(t, remote, "config", "user.email", "t@t")
	mustGit(t, remote, "config", "user.name", "t")
	mustGit(t, remote, "add", "-A")
	mustGit(t, remote, "commit", "-q", "-m", "share")
	return remote
}

// importFixtureGeneration runs the NEW generation-publish import from a
// materialized fixture dir (memories/*.md.age + share.json) under the import
// lease, committing a generation — the test analogue of a git/bucket pull for
// serving-layer tests that don't need to exercise real git.
func importFixtureGeneration(ctx context.Context, cfg Config, sub shareSubscription, dir string) (shareImportStats, error) {
	var stats shareImportStats
	err := shareBuildAndPublish(ctx, cfg, sub.Name, buildModeImport, func(runID string) (int, error) {
		man, merr := readShareManifest(dir)
		if merr != nil {
			return 0, merr
		}
		entries, derr := decryptShareBlobs(cfg, man, bucketDirBlobs(dir))
		if derr != nil {
			return 0, derr
		}
		seq, st, berr := buildAndCommitGeneration(ctx, cfg, sub, runID, "fixture", entries, shareCommitParams{parentFloor: -1})
		st.Owner, st.Scope = man.Owner, man.Scope
		stats = st
		return seq, berr
	})
	return stats, err
}

// resolveGenIndexRO resolves the published generation and opens its index.db with
// the committed integrity digest, failing the test if nothing resolves.
func resolveGenIndexRO(t *testing.T, cfg Config, name string) *sql.DB {
	t.Helper()
	c, ok, err := resolvePublishedCommit(cfg, name)
	if err != nil || !ok {
		t.Fatalf("resolvePublishedCommit(%q) = ok %v, err %v", name, ok, err)
	}
	db, err := openShareIndexRO(context.Background(), shareGenIndexPath(cfg, name, c.Gen), c.IndexDigest)
	if err != nil {
		t.Fatalf("openShareIndexRO: %v", err)
	}
	return db
}

// treeDigest hashes every file under root (path + content), for before/after
// no-mutation assertions.
func treeDigest(t *testing.T, root string) string {
	t.Helper()
	h := sha256.New()
	err := filepath.WalkDir(root, func(path string, d iofs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		fmt.Fprintf(h, "%s:%x\n", path, sum)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func TestShareImportDecryptsIndexesAndNeverTouchesVault(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	seedAuthored(t, "personal", "My own note", "local content")
	id := writeTestIdentity(t, cfg)

	sub := shareSubscription{Name: "neil", Remote: "git@example.test:me/vault.git", CreatedAt: "2026-07-01T00:00:00Z"}
	buildShareRepoFixture(t, shareRepoDir(cfg, "neil"), id.Recipient(),
		[]Memory{
			fixtureMemory("mem_20260601_000000_aaaaaaaa", "Shared decision", "we standardized on age"),
			fixtureMemory("mem_20260601_000001_bbbbbbbb", "Shared deadline", "beta ships in july"),
		}, true)

	vaultBefore := treeDigest(t, cfg.VaultDir)
	indexBefore := treeDigest(t, cfg.DataDir+"/index.db")

	stats, err := importFixtureGeneration(context.Background(), cfg, sub, shareRepoDir(cfg, sub.Name))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if stats.Imported != 2 || stats.Total != 2 || stats.Owner != "Adit" {
		t.Fatalf("stats = %+v; want 2 imported, owner Adit", stats)
	}

	// Corpus decrypted into the published generation, indexed in its own index.
	commit, ok, cerr := resolvePublishedCommit(cfg, "neil")
	if cerr != nil || !ok {
		t.Fatalf("no published generation: ok %v err %v", ok, cerr)
	}
	if _, err := os.Stat(filepath.Join(shareGenCorpusDir(cfg, "neil", commit.Gen), "mem_20260601_000000_aaaaaaaa.md")); err != nil {
		t.Fatalf("corpus file missing: %v", err)
	}
	db := resolveGenIndexRO(t, cfg, "neil")
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM memories`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("share index rows = %d, %v; want 2", n, err)
	}

	// The subscriber's own vault and personal index are byte-identical (AC5).
	if got := treeDigest(t, cfg.VaultDir); got != vaultBefore {
		t.Fatal("import mutated the subscriber's vault")
	}
	if got := treeDigest(t, cfg.DataDir+"/index.db"); got != indexBefore {
		t.Fatal("import mutated the subscriber's personal index")
	}
}

func TestShareImportRefusesIDSpoof(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	id := writeTestIdentity(t, cfg)
	sub := shareSubscription{Name: "neil", Remote: "r", CreatedAt: "2026-07-01T00:00:00Z"}

	spoof := fixtureMemory("mem_20260601_000000_cccccccc", "Spoof", "content")
	buildShareRepoFixture(t, shareRepoDir(cfg, "neil"), id.Recipient(), nil, true)
	body, err := renderMemory(spoof)
	if err != nil {
		t.Fatal(err)
	}
	ct, err := encryptShareBytes([]age.Recipient{id.Recipient()}, body)
	if err != nil {
		t.Fatal(err)
	}
	// Filename claims one id; the decrypted frontmatter claims another.
	if err := os.WriteFile(filepath.Join(shareRepoDir(cfg, "neil"), "memories", "mem_20260601_000000_dddddddd.md.age"), ct, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := importFixtureGeneration(context.Background(), cfg, sub, shareRepoDir(cfg, sub.Name)); err == nil || !strings.Contains(err.Error(), "id") {
		t.Fatalf("id-spoofed import = %v; want refusal", err)
	}
}

func TestShareImportPrunesRemovedMemories(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	id := writeTestIdentity(t, cfg)
	sub := shareSubscription{Name: "neil", Remote: "r", CreatedAt: "2026-07-01T00:00:00Z"}

	m1 := fixtureMemory("mem_20260601_000000_aaaaaaaa", "Keep", "kept")
	m2 := fixtureMemory("mem_20260601_000001_bbbbbbbb", "Drop", "dropped")
	buildShareRepoFixture(t, shareRepoDir(cfg, "neil"), id.Recipient(), []Memory{m1, m2}, true)
	if _, err := importFixtureGeneration(context.Background(), cfg, sub, shareRepoDir(cfg, sub.Name)); err != nil {
		t.Fatal(err)
	}
	// The publisher drops a memory; re-import builds a NEW immutable generation
	// from the current repo (never mutating the old one) that simply excludes it.
	if err := os.Remove(filepath.Join(shareRepoDir(cfg, "neil"), "memories", m2.ID+".md.age")); err != nil {
		t.Fatal(err)
	}
	stats, err := importFixtureGeneration(context.Background(), cfg, sub, shareRepoDir(cfg, sub.Name))
	if err != nil {
		t.Fatal(err)
	}
	if stats.Total != 1 {
		t.Fatalf("stats after removal = %+v; want 1 total", stats)
	}
	// The dropped memory is no longer served on any surface.
	if _, ok := findSharedMemory(cfg, m2.ID); ok {
		t.Fatal("removed memory still served — the new generation must exclude it")
	}
	db := resolveGenIndexRO(t, cfg, "neil")
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM memories`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("share index rows = %d, %v; want 1", n, err)
	}
}

func TestShareSubscribeRequiresIdentity(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	fx := &fakeExec{out: map[string]string{}, errOn: map[string]error{}}
	var buf bytes.Buffer
	err := shareSubscribe(context.Background(), cfg, []string{"neil", "--remote", "git@example.test:me/vault.git"}, &buf, fx.run)
	if err == nil || !strings.Contains(err.Error(), "keygen") {
		t.Fatalf("subscribe without identity = %v; want keygen pointer", err)
	}
	if fx.sawSubcommand("git", "clone") {
		t.Fatal("clone ran without a decryption identity")
	}
}

func TestShareSubscribeImportsExistingCloneAndSaves(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	id := writeTestIdentity(t, cfg)
	remote := realGitShareRemote(t, id.Recipient(),
		[]Memory{fixtureMemory("mem_20260601_000000_aaaaaaaa", "Shared", "content")})

	var buf bytes.Buffer
	if err := shareSubscribe(context.Background(), cfg, []string{"neil", "--remote", remote}, &buf, realExec); err != nil {
		t.Fatalf("shareSubscribe: %v\n%s", err, buf.String())
	}
	sf, err := loadShares(cfg)
	if err != nil || len(sf.Subscriptions) != 1 || sf.Subscriptions[0].Name != "neil" {
		t.Fatalf("subscription not saved: %+v %v", sf, err)
	}
	if !strings.Contains(buf.String(), "Adit") || !strings.Contains(buf.String(), "1") {
		t.Fatalf("subscribe output missing owner/count:\n%s", buf.String())
	}
	// The first pull minted and committed a generation, now served.
	if _, ok := findSharedMemory(cfg, "mem_20260601_000000_aaaaaaaa"); !ok {
		t.Fatal("subscribed memory not served from the committed generation")
	}
}

func TestShareSubscribeClonesWhenMissing(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	writeTestIdentity(t, cfg)

	// A real remote with a commit but NO share.json: the clone succeeds and the
	// pin resolves, but reading the manifest from the pinned tree fails loudly.
	remote := t.TempDir()
	if err := os.WriteFile(filepath.Join(remote, "readme"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, remote, "init", "-q")
	mustGit(t, remote, "config", "user.email", "t@t")
	mustGit(t, remote, "config", "user.name", "t")
	mustGit(t, remote, "add", "-A")
	mustGit(t, remote, "commit", "-q", "-m", "no-manifest")

	var buf bytes.Buffer
	err := shareSubscribe(context.Background(), cfg, []string{"neil", "--remote", remote}, &buf, realExec)
	if err == nil {
		t.Fatal("subscribe succeeded despite a repo with no share.json; want manifest error")
	}
	// The failed fresh subscription leaves no registered clone behind. The
	// durable failed attempt remains for doctor/retry visibility.
	if _, ierr := os.Stat(shareRepoDir(cfg, "neil")); !os.IsNotExist(ierr) {
		t.Fatal("failed fresh subscribe left its clone behind")
	}
	sf, lerr := loadShares(cfg)
	if lerr != nil || len(sf.Subscriptions) != 0 {
		t.Fatalf("failed fresh subscribe registered state: %+v %v", sf, lerr)
	}
	attempt, ok, aerr := loadShareAttempt(cfg, "neil")
	if aerr != nil || !ok || attempt.State != "failed" {
		t.Fatalf("failed fresh subscribe lost durable failure evidence: %+v ok=%v err=%v", attempt, ok, aerr)
	}
}

func TestConcurrentFirstSubscribersSerializeCloneAndRegistration(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	id := writeTestIdentity(t, cfg)
	mem := fixtureMemory("mem_20260601_000000_aaaaaaaa", "Shared", "content")
	remote := realGitShareRemote(t, id.Recipient(), []Memory{mem})

	// Hold the global chokepoint until both callers have performed their
	// optimistic preflight. The eventual winner must clone+register while locked;
	// the waiter must re-read that state instead of acting on stale isRepo=false.
	holdRelease, err := acquireStorageLease(cfg, "test-holder", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		err error
		out string
	}
	started := make(chan struct{}, 2)
	results := make(chan result, 2)
	for range 2 {
		go func() {
			started <- struct{}{}
			var out bytes.Buffer
			err := shareSubscribe(context.Background(), cfg, []string{"neil", "--remote", remote}, &out, realExec)
			results <- result{err: err, out: out.String()}
		}()
	}
	<-started
	<-started
	time.Sleep(100 * time.Millisecond)
	holdRelease()
	r1, r2 := <-results, <-results

	successes := 0
	conflicts := 0
	for _, got := range []result{r1, r2} {
		if got.err == nil {
			successes++
		} else if strings.Contains(got.err.Error(), "already exists") {
			conflicts++
		} else {
			t.Fatalf("concurrent subscribe failed unexpectedly: %v\n%s", got.err, got.out)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent outcomes: successes=%d conflicts=%d; want one each", successes, conflicts)
	}
	sf, err := loadShares(cfg)
	if err != nil || len(sf.Subscriptions) != 1 || sf.Subscriptions[0].Name != "neil" {
		t.Fatalf("serialized registration = %+v, %v; want one neil", sf, err)
	}
	if _, ok := findSharedMemory(cfg, mem.ID); !ok {
		t.Fatal("losing first-subscriber removed the winner's committed generation")
	}
	if h := shareHealthOne(cfg, "neil", time.Now()); h.State != healthFresh {
		t.Fatalf("losing first-subscriber poisoned winner health: %+v", h)
	}
}

func TestSharePullReimportsAfterPublisherUpdate(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	id := writeTestIdentity(t, cfg)
	remote := realGitShareRemote(t, id.Recipient(),
		[]Memory{fixtureMemory("mem_20260601_000000_aaaaaaaa", "Shared", "content")})
	var buf bytes.Buffer
	if err := shareSubscribe(context.Background(), cfg, []string{"neil", "--remote", remote}, &buf, realExec); err != nil {
		t.Fatalf("subscribe: %v\n%s", err, buf.String())
	}

	// Publisher adds a second memory and commits.
	body, err := renderMemory(fixtureMemory("mem_20260601_000001_bbbbbbbb", "Second", "more"))
	if err != nil {
		t.Fatal(err)
	}
	ct, err := encryptShareBytes([]age.Recipient{id.Recipient()}, body)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remote, "memories", "mem_20260601_000001_bbbbbbbb.md.age"), ct, 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, remote, "add", "-A")
	mustGit(t, remote, "commit", "-q", "-m", "add second")

	buf.Reset()
	if err := sharePull(context.Background(), cfg, nil, &buf, realExec); err != nil {
		t.Fatalf("sharePull: %v\n%s", err, buf.String())
	}
	if _, ok := findSharedMemory(cfg, "mem_20260601_000001_bbbbbbbb"); !ok {
		t.Fatal("pull did not import the publisher's new memory")
	}
}

// setupSubscription installs a ready-to-search subscription: fixture repo,
// decrypted corpus, share index, and registry entry.
func setupSubscription(t *testing.T, cfg Config, name string, mems []Memory) {
	t.Helper()
	id := writeTestIdentity(t, cfg)
	buildShareRepoFixture(t, shareRepoDir(cfg, name), id.Recipient(), mems, true)
	sub := shareSubscription{Name: name, Remote: "r", CreatedAt: "2026-07-01T00:00:00Z"}
	if _, err := importFixtureGeneration(context.Background(), cfg, sub, shareRepoDir(cfg, sub.Name)); err != nil {
		t.Fatal(err)
	}
	sf, err := loadShares(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sf.Subscriptions = append(sf.Subscriptions, sub)
	if err := saveShares(cfg, sf); err != nil {
		t.Fatal(err)
	}
}

func TestSearchUnionsSharedCorpusWithAttribution(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	seedAuthored(t, "personal", "Local sqlite note", "we picked sqlite locally")
	setupSubscription(t, cfg, "neil", []Memory{
		fixtureMemory("mem_20260601_000000_aaaaaaaa", "Neil sqlite decision", "neil standardized on sqlite too"),
	})

	out := run(t, "search", "sqlite")
	if !strings.Contains(out, "Local sqlite note") {
		t.Fatalf("local result missing:\n%s", out)
	}
	if !strings.Contains(out, "[neil] Neil sqlite decision") {
		t.Fatalf("shared result missing owner-prefixed attribution:\n%s", out)
	}
	jout := run(t, "search", "sqlite", "--json")
	if !strings.Contains(jout, `"owner": "neil"`) {
		t.Fatalf("json result missing owner field:\n%s", jout)
	}
}

func TestSearchScopeFilterAppliesToSharedCorpus(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	seedAuthored(t, "personal", "Local sqlite note", "we picked sqlite locally")
	setupSubscription(t, cfg, "neil", []Memory{
		fixtureMemory("mem_20260601_000000_aaaaaaaa", "Neil sqlite decision", "neil standardized on sqlite too"),
	})
	out := run(t, "search", "sqlite", "--scope", "personal")
	if strings.Contains(out, "neil") {
		t.Fatalf("scope filter leaked shared scope project:acme:\n%s", out)
	}
}

func TestSearchSkipsNeverPulledSubscription(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	seedAuthored(t, "personal", "Local sqlite note", "we picked sqlite locally")
	sf, err := loadShares(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sf.Subscriptions = append(sf.Subscriptions, shareSubscription{Name: "ghost", Remote: "r", CreatedAt: "2026-07-01T00:00:00Z"})
	if err := saveShares(cfg, sf); err != nil {
		t.Fatal(err)
	}
	out := run(t, "search", "sqlite")
	if !strings.Contains(out, "Local sqlite note") || strings.Contains(out, "ghost") {
		t.Fatalf("never-pulled subscription broke or polluted search:\n%s", out)
	}
}

// Per-artifact suppression (H5): a subscription with no valid committed
// generation is EXCLUDED from search, but one bad subscription never takes down
// local-vault recall — and doctor surfaces it as failed.
func TestSearchCorruptShareIndexNeverTakesDownLocalRecall(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	seedAuthored(t, "personal", "Local sqlite note", "we picked sqlite locally")
	sf, err := loadShares(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sf.Subscriptions = append(sf.Subscriptions, shareSubscription{Name: "bad", Remote: "r", CreatedAt: "2026-07-01T00:00:00Z"})
	if err := saveShares(cfg, sf); err != nil {
		t.Fatal(err)
	}
	// A legacy flat garbage layout with no committed generation: fail-closed.
	if err := os.MkdirAll(shareSubRoot(cfg, "bad"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shareIndexPath(cfg, "bad"), []byte("not a database"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Local search still works; the bad share is silently excluded (never served,
	// never crashing the whole search).
	out := run(t, "search", "sqlite")
	if !strings.Contains(out, "Local sqlite note") || strings.Contains(out, "[bad]") {
		t.Fatalf("a bad share broke or polluted local search:\n%s", out)
	}
	// Doctor surfaces the unhealthy subscription (fail-closed visibility).
	h := shareHealthOne(cfg, "bad", time.Now())
	if h.State == healthFresh {
		t.Fatalf("bad share reported fresh; want failed/never, got %s", h.State)
	}
}

func TestMCPSearchMemoryCarriesOwner(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	seedAuthored(t, "personal", "Local sqlite note", "we picked sqlite locally")
	setupSubscription(t, cfg, "neil", []Memory{
		fixtureMemory("mem_20260601_000000_aaaaaaaa", "Neil sqlite decision", "neil standardized on sqlite too"),
	})
	got, err := callMCPTool(context.Background(), "search_memory", map[string]any{"query": "sqlite"})
	if err != nil {
		t.Fatalf("search_memory: %v", err)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"owner":"neil"`) {
		t.Fatalf("MCP search_memory result missing owner attribution:\n%s", b)
	}
}

func TestThinkAttributesSharedEvidenceAndKeepsGapsLocal(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	seedAuthored(t, "personal", "Local sqlite note", "we picked sqlite locally")
	setupSubscription(t, cfg, "neil", []Memory{
		fixtureMemory("mem_20260601_000000_aaaaaaaa", "Neil sqlite decision", "neil standardized on sqlite too"),
	})
	res, err := buildThink(context.Background(), cfg, "sqlite decision", "", 8, time.Now())
	if err != nil {
		t.Fatalf("buildThink: %v", err)
	}
	var shared int
	for _, e := range res.Evidence {
		if e.Owner == "neil" {
			shared++
		}
	}
	if shared == 0 {
		t.Fatalf("think evidence missing shared attribution: %+v", res.Evidence)
	}
	if !strings.Contains(res.SynthesisPrompt, "shared:neil") {
		t.Fatalf("synthesis prompt does not label shared evidence:\n%s", res.SynthesisPrompt)
	}
}

func TestShareListShowsPublishesAndSubscriptions(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	setupPublish(t, cfg, "acme", "project:acme", testRecipient(t))
	setupSubscription(t, cfg, "neil", []Memory{
		fixtureMemory("mem_20260601_000000_aaaaaaaa", "Shared", "content"),
	})

	out := run(t, "share", "list")
	for _, want := range []string{"acme", "project:acme", "neil"} {
		if !strings.Contains(out, want) {
			t.Fatalf("share list missing %q:\n%s", want, out)
		}
	}
	var rep struct {
		Publishes []struct {
			Name       string `json:"name"`
			Scope      string `json:"scope"`
			Recipients int    `json:"recipients"`
		} `json:"publishes"`
		Subscriptions []struct {
			Name     string `json:"name"`
			Memories int    `json:"memories"`
		} `json:"subscriptions"`
	}
	jout := run(t, "share", "list", "--json")
	if err := json.Unmarshal([]byte(jout), &rep); err != nil {
		t.Fatalf("share list --json: %v\n%s", err, jout)
	}
	if len(rep.Publishes) != 1 || rep.Publishes[0].Recipients != 1 ||
		len(rep.Subscriptions) != 1 || rep.Subscriptions[0].Memories != 1 {
		t.Fatalf("share list --json wrong: %+v", rep)
	}
}

func TestShareRemoveRequiresYes(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	setupPublish(t, cfg, "acme", "project:acme", testRecipient(t))
	var buf bytes.Buffer
	err := Run(context.Background(), []string{"share", "remove", "acme"}, &buf, &buf, strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("remove without --yes = %v; want confirm requirement", err)
	}
	if sf, _ := loadShares(cfg); len(sf.Publishes) != 1 {
		t.Fatal("publish removed without confirmation")
	}
}

func TestShareRemovePublishIsHonestAboutRevocation(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	setupPublish(t, cfg, "acme", "project:acme", testRecipient(t))
	out := run(t, "share", "remove", "acme", "--yes")
	if !strings.Contains(out, "already pulled") {
		t.Fatalf("remove output not honest about durable git history:\n%s", out)
	}
	if sf, _ := loadShares(cfg); len(sf.Publishes) != 0 {
		t.Fatal("publish still registered after remove")
	}
	if _, err := os.Stat(shareStagingDir(cfg, "acme")); !os.IsNotExist(err) {
		t.Fatal("staging repo not deleted")
	}
}

func TestShareRemoveSubscriptionDeletesShareRoot(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	setupSubscription(t, cfg, "neil", []Memory{
		fixtureMemory("mem_20260601_000000_aaaaaaaa", "Shared", "content"),
	})
	run(t, "share", "remove", "neil", "--yes")
	if sf, _ := loadShares(cfg); len(sf.Subscriptions) != 0 {
		t.Fatal("subscription still registered after remove")
	}
	if _, err := os.Stat(shareSubRoot(cfg, "neil")); !os.IsNotExist(err) {
		t.Fatal("share root (corpus+index) not deleted")
	}
}

func TestDoctorReportsShareState(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	setupPublish(t, cfg, "acme", "project:acme", testRecipient(t))

	var rep struct {
		SharePublishes     int `json:"share_publishes"`
		ShareSubscriptions int `json:"share_subscriptions"`
		Checks             []struct {
			Name string `json:"name"`
			OK   bool   `json:"ok"`
		} `json:"checks"`
	}
	stagingCheck := func() (found, ok bool) {
		out := run(t, "doctor", "--json")
		if err := json.Unmarshal([]byte(out), &rep); err != nil {
			t.Fatalf("doctor --json: %v\n%s", err, out)
		}
		for _, c := range rep.Checks {
			if c.Name == "share_staging_clean" {
				return true, c.OK
			}
		}
		return false, false
	}
	found, ok := stagingCheck()
	if !found || !ok || rep.SharePublishes != 1 {
		t.Fatalf("doctor missing healthy share state (found=%v ok=%v pubs=%d)", found, ok, rep.SharePublishes)
	}
	// Plant plaintext markdown in the staging repo — the check must flip.
	leak := filepath.Join(shareStagingDir(cfg, "acme"), "memories", "leak.md")
	if err := os.MkdirAll(filepath.Dir(leak), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(leak, []byte("plaintext"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := stagingCheck(); ok {
		t.Fatal("share_staging_clean stayed ok with plaintext .md in staging")
	}
	// Human output discloses the share egress surface.
	out := run(t, "doctor")
	if !strings.Contains(out, "share") || !strings.Contains(strings.ToUpper(out), "PRIVATE") {
		t.Fatalf("doctor text missing share disclosure:\n%s", out)
	}
}

func TestUsageMentionsShare(t *testing.T) {
	withTempHome(t)
	out := run(t)
	if !strings.Contains(out, "mora share") {
		t.Fatalf("printUsage does not mention share:\n%s", out)
	}
}

// --github wires origin via gh, so the registry must record the RESOLVED origin
// URL — otherwise the push-time origin-vs-registry check has nothing to compare
// against and a swapped origin goes unnoticed (codex review P1).
func TestShareInitGithubRecordsResolvedRemote(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	fx := &fakeExec{out: map[string]string{}, errOn: map[string]error{}}
	var buf bytes.Buffer
	err := shareInit(context.Background(), cfg,
		[]string{"acme", "--scope", "project:acme", "--recipient", testRecipient(t), "--github"},
		&buf, fx.run)
	if err != nil {
		t.Fatalf("shareInit --github: %v", err)
	}
	sf, err := loadShares(cfg)
	if err != nil || len(sf.Publishes) != 1 {
		t.Fatal(err)
	}
	if sf.Publishes[0].Remote == "" {
		t.Fatal("--github publish saved with empty Remote — push-time origin verification is disabled")
	}
}

// A pre-existing clone dir for the subscription name must have its origin
// verified against --remote, or a stale/wrong clone gets imported under a
// trusted attribution label (codex review P1).
func TestShareSubscribeVerifiesExistingCloneOrigin(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	id := writeTestIdentity(t, cfg)
	buildShareRepoFixture(t, shareRepoDir(cfg, "neil"), id.Recipient(),
		[]Memory{fixtureMemory("mem_20260601_000000_aaaaaaaa", "Shared", "content")}, true)

	fx := &fakeExec{out: map[string]string{}, errOn: map[string]error{}, hasOrigin: true} // origin = git@example.test:me/vault.git
	var buf bytes.Buffer
	err := shareSubscribe(context.Background(), cfg, []string{"neil", "--remote", "git@evil.test:someone/else.git"}, &buf, fx.run)
	if err == nil || !strings.Contains(err.Error(), "origin") {
		t.Fatalf("subscribe over mismatched existing clone = %v; want origin refusal", err)
	}
	if sf, _ := loadShares(cfg); len(sf.Subscriptions) != 0 {
		t.Fatal("mismatched subscription was saved")
	}
}

// Ciphertext size is bounded BEFORE the file is read into memory — a hostile
// repo must not be able to exhaust RAM via one huge .age file (codex review P1).
func TestShareImportRefusesOversizedCiphertext(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	id := writeTestIdentity(t, cfg)
	buildShareRepoFixture(t, shareRepoDir(cfg, "neil"), id.Recipient(), nil, true)
	big := filepath.Join(shareRepoDir(cfg, "neil"), "memories", "mem_20260601_000000_ffffffff.md.age")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(shareMaxMemoryBytes + (2 << 20)); err != nil {
		t.Fatal(err)
	}
	f.Close()
	sub := shareSubscription{Name: "neil", Remote: "r", CreatedAt: "2026-07-01T00:00:00Z"}
	_, err = importFixtureGeneration(context.Background(), cfg, sub, shareRepoDir(cfg, sub.Name))
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized ciphertext = %v; want size refusal", err)
	}
}

// The tracked-file check must be an ALLOWLIST: `git add -A` stages anything in
// the staging tree, and a denylist lets a stray secrets.env ride a push without
// ever appearing in the preview (review finding).
func TestSharePushRefusesTrackedFileOutsideAllowlist(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	seedAuthored(t, "project:acme", "Note", "content")
	setupPublish(t, cfg, "acme", "project:acme", testRecipient(t))

	fx := &fakeExec{out: map[string]string{
		"git ls-files": ".gitignore\nshare.json\nmemories/mem_20260601_000000_aaaaaaaa.md.age\nsecrets.env\n",
	}, errOn: map[string]error{}, hasOrigin: true}
	var buf bytes.Buffer
	err := sharePush(context.Background(), cfg, []string{"acme", "--yes"}, &buf, strings.NewReader(""), fx.run)
	if err == nil || !strings.Contains(err.Error(), "secrets.env") {
		t.Fatalf("stray tracked file = %v; want allowlist refusal naming it", err)
	}
	if fx.sawSubcommand("git", "commit") || fx.sawSubcommand("git", "push") {
		t.Fatal("commit/push ran past the allowlist stop")
	}
}

// Companion: allowlisted-only tracked files pass — proves the check is an
// allowlist, not "refuse when ls-files prints anything".
func TestSharePushAcceptsAllowlistedTrackedFiles(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	seedAuthored(t, "project:acme", "Note", "content")
	setupPublish(t, cfg, "acme", "project:acme", testRecipient(t))

	fx := &fakeExec{out: map[string]string{
		"git ls-files": ".gitignore\nshare.json\nmemories/mem_20260601_000000_aaaaaaaa.md.age\n",
		"git status":   " M x",
	}, errOn: map[string]error{}, hasOrigin: true}
	var buf bytes.Buffer
	if err := sharePush(context.Background(), cfg, []string{"acme", "--yes"}, &buf, strings.NewReader(""), fx.run); err != nil {
		t.Fatalf("allowlisted tracked files refused: %v", err)
	}
	if !fx.sawSubcommand("git", "push", "origin", "HEAD") {
		t.Fatal("push skipped")
	}
}

// A staged ciphertext that vanished (crash, git clean, partial restore) while
// the push state still lists it must be RE-PUBLISHED, not silently pushed as an
// unpreviewed deletion (review finding, live-repro confirmed).
func TestSharePushReaddsMissingStagedCiphertext(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	memID := seedAuthored(t, "project:acme", "Keep", "kept content")
	setupPublish(t, cfg, "acme", "project:acme", id.Recipient().String())

	fx := &fakeExec{out: map[string]string{"git status": " M x"}, errOn: map[string]error{}, hasOrigin: true}
	var buf bytes.Buffer
	if err := sharePush(context.Background(), cfg, []string{"acme", "--yes"}, &buf, strings.NewReader(""), fx.run); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(shareStagingDir(cfg, "acme"), "memories", memID+".md.age")
	if err := os.Remove(staged); err != nil {
		t.Fatal(err)
	}
	var buf2 bytes.Buffer
	fx2 := &fakeExec{out: map[string]string{"git status": " M x"}, errOn: map[string]error{}, hasOrigin: true}
	if err := sharePush(context.Background(), cfg, []string{"acme", "--yes"}, &buf2, strings.NewReader(""), fx2.run); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf2.String(), "no changes") {
		t.Fatalf("push claimed no changes while a staged file was missing:\n%s", buf2.String())
	}
	if _, err := os.Stat(staged); err != nil {
		t.Fatal("missing staged ciphertext was not re-encrypted")
	}
}

// Publish side enforces the same per-memory size cap the import side does — one
// oversized memory must fail at push (publisher can fix it), not wedge every
// subscriber's pull (review finding).
func TestCollectShareMemoriesRejectsOversizedMemory(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	big := Memory{ID: "mem_20260101_000000_bbbbbbbb", Scope: "project:acme", Type: "insight",
		Title: "Huge", CreatedAt: "2026-01-01T00:00:00Z", Text: strings.Repeat("x", shareMaxMemoryBytes+1)}
	if err := writeMemory(cfg, big); err != nil {
		t.Fatal(err)
	}
	_, err := collectShareMemories(cfg, "project:acme")
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized memory = %v; want size refusal at export", err)
	}
}

// Ids differing only by letter case collide on case-insensitive filesystems
// (macOS/Windows subscriber corpora) — refuse at export so the publisher can
// rename, instead of wedging subscribers (review finding).
func TestCollectShareMemoriesRejectsCaseFoldDuplicateIDs(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	// The two files live in DIFFERENT directories (scope matching is
	// frontmatter-authoritative) so this test itself survives a case-
	// insensitive dev filesystem.
	files := map[string]string{
		"mem_20260101_000000_aaaaaaAA": filepath.Join(cfg.VaultDir, "memories", "project", "acme"),
		"mem_20260101_000000_AAAAAAaa": filepath.Join(cfg.VaultDir, "memories", "global"),
	}
	for id, dir := range files {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		body := "---\nid: " + id + "\nscope: project:acme\ntype: insight\ntitle: T\ntags: []\nsource: manual\ncreated_at: 2026-01-01T00:00:00Z\n---\n\nx\n"
		if err := os.WriteFile(filepath.Join(dir, id+".md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, err := collectShareMemories(cfg, "project:acme")
	if err == nil || !strings.Contains(err.Error(), "case") {
		t.Fatalf("case-fold duplicate ids = %v; want refusal naming the collision", err)
	}
}

// A failed FIRST import (e.g. subscribing before the publisher's first push)
// must clean up the fresh clone so retrying works. The root itself deliberately
// remains because attempt.json is the durable failure evidence.
func TestShareSubscribeCleansUpFreshCloneOnImportFailure(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	writeTestIdentity(t, cfg)

	fx := &fakeExec{out: map[string]string{}, errOn: map[string]error{}}
	var buf bytes.Buffer
	err := shareSubscribe(context.Background(), cfg, []string{"neil", "--remote", "git@example.test:me/vault.git"}, &buf, fx.run)
	if err == nil {
		t.Fatal("empty clone import unexpectedly succeeded")
	}
	if _, statErr := os.Stat(shareRepoDir(cfg, "neil")); !os.IsNotExist(statErr) {
		t.Fatal("failed fresh subscribe left an orphan clone that blocks retries")
	}
	// Retry attempts a fresh clone rather than reusing a stale dir.
	fx2 := &fakeExec{out: map[string]string{}, errOn: map[string]error{}}
	_ = shareSubscribe(context.Background(), cfg, []string{"neil", "--remote", "git@example.test:me/vault.git"}, &buf, fx2.run)
	if !fx2.sawSubcommand("git", "clone") {
		t.Fatal("retry did not re-clone")
	}
}

// Re-subscribing over an existing clone must freshen it (pull --ff-only), not
// import whatever stale state the clone holds (review finding).
// The published generation's index.db is immutable, but on-disk corruption is
// still possible — search heals by re-cutting a repair generation from the head's
// OWN frozen corpus (never bricking recall) and keeps serving.
func TestShareIndexSelfHealsFromCorpus(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	seedAuthored(t, "personal", "Local sqlite note", "we picked sqlite locally")
	setupSubscription(t, cfg, "neil", []Memory{
		fixtureMemory("mem_20260601_000000_aaaaaaaa", "Neil sqlite decision", "neil standardized on sqlite too"),
	})
	// Corrupt the PUBLISHED generation's index.db outright.
	commit, ok, err := resolvePublishedCommit(cfg, "neil")
	if err != nil || !ok {
		t.Fatalf("no published gen: %v", err)
	}
	if err := os.WriteFile(shareGenIndexPath(cfg, "neil", commit.Gen), []byte("not a database"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := run(t, "search", "sqlite")
	if !strings.Contains(out, "[neil] Neil sqlite decision") {
		t.Fatalf("search did not heal the corrupt share index from the frozen corpus:\n%s", out)
	}
}

// Shared ids that search returns must be resolvable to full text via `mora
// read` and MCP read_memory — snippets are truncated at 240 runes and there was
// no expansion path (review finding). delete stays vault-only.
func TestReadResolvesSharedMemoryButDeleteDoesNot(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	longText := "neil standardized on sqlite too " + strings.Repeat("padding words here ", 30)
	setupSubscription(t, cfg, "neil", []Memory{
		fixtureMemory("mem_20260601_000000_aaaaaaaa", "Neil sqlite decision", longText),
	})
	out := run(t, "read", "mem_20260601_000000_aaaaaaaa", "--json")
	if !strings.Contains(out, `"owner": "neil"`) || !strings.Contains(out, "padding words here") {
		t.Fatalf("read did not resolve the shared memory with full text + owner:\n%s", out)
	}
	got, err := callMCPTool(context.Background(), "read_memory", map[string]any{"id": "mem_20260601_000000_aaaaaaaa"})
	if err != nil {
		t.Fatalf("MCP read_memory on shared id: %v", err)
	}
	b, _ := json.Marshal(got)
	if !strings.Contains(string(b), `"owner":"neil"`) {
		t.Fatalf("MCP read_memory missing owner:\n%s", b)
	}
	var buf bytes.Buffer
	err = Run(context.Background(), []string{"delete", "mem_20260601_000000_aaaaaaaa", "--yes"}, &buf, &buf, strings.NewReader(""))
	if err == nil {
		t.Fatal("delete reached a shared memory; shares are read-only")
	}
}

// A corrupt shares.json must fail search with an ACTIONABLE error (naming the
// file) and flip a critical doctor check — not a bare JSON parse message with
// doctor reporting healthy (review finding, live-repro).
func TestCorruptSharesRegistryIsActionable(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	seedAuthored(t, "personal", "Local note", "content")
	if err := os.WriteFile(sharesPath(cfg), []byte("{ bad json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	err := Run(context.Background(), []string{"search", "content"}, &buf, &buf, strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "shares.json") {
		t.Fatalf("corrupt registry error not actionable: %v", err)
	}
	var rep struct {
		Healthy bool `json:"healthy"`
		Checks  []struct {
			Name string `json:"name"`
			OK   bool   `json:"ok"`
		} `json:"checks"`
	}
	out := run(t, "doctor", "--json")
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range rep.Checks {
		if c.Name == "shares_registry_readable" {
			found = true
			if c.OK {
				t.Fatal("shares_registry_readable ok despite corrupt file")
			}
		}
	}
	if !found || rep.Healthy {
		t.Fatalf("doctor healthy=%v, check found=%v; want unhealthy + check present", rep.Healthy, found)
	}
}

// When only shared corpora match, the local-only gap analysis must say so
// instead of asserting "No memory matched this query" beside shared evidence
// (review finding).
func TestThinkGapWordingWithOnlySharedEvidence(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	setupSubscription(t, cfg, "neil", []Memory{
		fixtureMemory("mem_20260601_000000_aaaaaaaa", "Acme widgets", "acme ships widgets in q3"),
	})
	res, err := buildThink(context.Background(), cfg, "acme widgets", "", 8, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Evidence) == 0 {
		t.Fatal("expected shared evidence")
	}
	joined := strings.Join(res.Gaps.CoverageHoles, " | ")
	if strings.Contains(joined, "No memory matched this query.") {
		t.Fatalf("gap contradicts shared evidence: %q", joined)
	}
	if !strings.Contains(joined, "your own vault") {
		t.Fatalf("gap does not clarify the vault-vs-shared distinction: %q", joined)
	}
}

// `mora share keygen --help` must print usage, not mint an identity (review
// finding, live-repro: it wrote identity.txt and exited 0).
func TestShareKeygenHelpHasNoSideEffects(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	out := run(t, "share", "keygen", "--help")
	if !strings.Contains(strings.ToLower(out), "usage: mora share") {
		t.Fatalf("keygen --help did not print usage:\n%s", out)
	}
	if _, err := os.Stat(shareIdentityPath(cfg)); !os.IsNotExist(err) {
		t.Fatal("keygen --help minted an identity")
	}
	var buf bytes.Buffer
	if err := Run(context.Background(), []string{"share", "keygen", "stray"}, &buf, &buf, strings.NewReader("")); err == nil {
		t.Fatal("keygen with stray args accepted")
	}
}

// The vault egress paths must know about the NEW secret class this feature
// introduces: the age identity (the only key that decrypts shares sent to this
// user) and share dirs holding decrypted foreign corpora. If config drift ever
// co-locates them with the vault, git-sync must shield/refuse them (review
// finding: vault_dir flips have happened three times in production).
func TestVaultGitSyncShieldsShareSecrets(t *testing.T) {
	if !strings.Contains(gitignoreBody, "identity*") || !strings.Contains(gitignoreBody, "share/") {
		t.Fatalf("vault .gitignore does not shield share identity/dirs:\n%s", gitignoreBody)
	}
	cfg := gitSyncTestConfig(t)
	if err := os.MkdirAll(filepath.Join(cfg.VaultDir, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	f := dirtyFake()
	f.hasOrigin = true
	f.out["git ls-files"] = "share/identity.txt\n"
	var out strings.Builder
	err := syncGit(context.Background(), cfg, nil, &out, f.run)
	if err == nil || !strings.Contains(err.Error(), "share/identity.txt") {
		t.Fatalf("tracked share identity must hard-stop the vault sync, got: %v", err)
	}
	sawIdentityGlob := false
	for _, c := range f.calls {
		if len(c) > 2 && c[1] == "ls-files" {
			for _, a := range c {
				if a == "identity*" {
					sawIdentityGlob = true
				}
			}
		}
	}
	if !sawIdentityGlob {
		t.Fatalf("vault sync ls-files hard-stop does not probe identity*; calls: %v", f.calls)
	}
}

// Doctor mirrors tokens_disjoint_from_vault for the share paths: a vault that
// engulfs the share root or identity dir is a critical data-egress hazard.
func TestDoctorChecksShareDisjointFromVault(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	var rep struct {
		Checks []struct {
			Name string `json:"name"`
			OK   bool   `json:"ok"`
		} `json:"checks"`
	}
	check := func() (found, ok bool) {
		out := run(t, "doctor", "--json")
		if err := json.Unmarshal([]byte(out), &rep); err != nil {
			t.Fatal(err)
		}
		for _, c := range rep.Checks {
			if c.Name == "share_disjoint_from_vault" {
				return true, c.OK
			}
		}
		return false, false
	}
	if found, ok := check(); !found || !ok {
		t.Fatalf("healthy layout: found=%v ok=%v; want present and ok", found, ok)
	}
	// Re-point the vault OVER the whole install root: share dirs + identity
	// would now live inside the vault.
	f, err := os.OpenFile(filepath.Join(cfg.ConfigDir, "config.toml"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("vault_dir = \"" + filepath.Dir(cfg.DataDir) + "\"\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, ok := check(); ok {
		t.Fatal("share_disjoint_from_vault stayed ok with the vault engulfing the share paths")
	}
}

// Import must also refuse case-fold id collisions: on a case-insensitive
// subscriber filesystem the corpus files would silently overwrite each other
// (review finding; publisher-side refusal already exists, this is the
// hostile-publisher backstop).
func TestShareImportRefusesCaseFoldCollision(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	id := writeTestIdentity(t, cfg)
	buildShareRepoFixture(t, shareRepoDir(cfg, "neil"), id.Recipient(), nil, true)
	for _, memID := range []string{"mem_20260601_000000_aaaaaaAA", "mem_20260601_000000_AAAAAAaa"} {
		m := fixtureMemory(memID, "T", "content")
		body, err := renderMemory(m)
		if err != nil {
			t.Fatal(err)
		}
		ct, err := encryptShareBytes([]age.Recipient{id.Recipient()}, body)
		if err != nil {
			t.Fatal(err)
		}
		// A case-insensitive dev filesystem would collapse these two repo file
		// names too — skip if the fixture itself cannot represent them.
		p := filepath.Join(shareRepoDir(cfg, "neil"), "memories", memID+".md.age")
		if err := os.WriteFile(p, ct, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(shareRepoDir(cfg, "neil"), "memories"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 2 {
		t.Skip("filesystem collapsed the fixture names; collision unrepresentable here")
	}
	sub := shareSubscription{Name: "neil", Remote: "r", CreatedAt: "2026-07-01T00:00:00Z"}
	if _, err := importFixtureGeneration(context.Background(), cfg, sub, shareRepoDir(cfg, sub.Name)); err == nil || !strings.Contains(err.Error(), "case") {
		t.Fatalf("case-fold collision import = %v; want refusal naming the collision", err)
	}
}

// The vault-disjointness guard must compare case-insensitively on filesystems
// that do (macOS/Windows): /Users/x/VAULT and /Users/x/vault are the same tree
// there, and a case-variant vault_dir must not slip past the prefix check
// (review finding).
func TestSharePathsOverlapCaseFold(t *testing.T) {
	if !sharePathsOverlap("/users/adit/vault/mora/data/share", "/Users/Adit/VAULT/mora", true) {
		t.Fatal("case-folded overlap not detected")
	}
	if sharePathsOverlap("/users/adit/vault/mora/data/share", "/Users/Adit/VAULT/mora", false) {
		t.Fatal("case-sensitive comparison must not fold")
	}
	if sharePathsOverlap("/a/b-data/share", "/a/b", true) {
		t.Fatal("sibling with shared name prefix wrongly flagged")
	}
	if !sharePathsOverlap("/a/b", "/a/b/vault", true) {
		t.Fatal("containment must be detected in both directions")
	}
}
