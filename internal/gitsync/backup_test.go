package gitsync

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRetryNeverInitializesCreatesOrRepoints(t *testing.T) {
	vault, remote, opts := backupFixture(t)
	writeGitFile(t, vault, "changed.md", "changed")
	if err := PushSelected(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if got := gitRun(t, vault, "remote", "get-url", "origin"); got != opts.Target.CanonicalURL {
		t.Fatalf("origin=%q", got)
	}
	if got := gitRun(t, vault, "rev-parse", "--is-inside-work-tree"); got != "true" {
		t.Fatalf("vault not repo: %q", got)
	}
	if _, err := os.Stat(remote); err != nil {
		t.Fatal(err)
	}
}

func TestDivergencePreservesBothHistories(t *testing.T) {
	vault, _, opts := backupFixture(t)
	other := cloneOther(t, opts.Target.CanonicalURL)
	writeGitFile(t, vault, "local-only.md", "local")
	gitRun(t, vault, "add", ".")
	gitRun(t, vault, "commit", "-m", "local")
	writeGitFile(t, other, "remote-only.md", "remote")
	gitRun(t, other, "add", ".")
	gitRun(t, other, "commit", "-m", "remote")
	gitRun(t, other, "push", "origin", "main")
	result, err := ProbeRelation(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if result.Relation != RelationDiverged || result.BundlePath == "" {
		t.Fatalf("relation=%#v", result)
	}
	gitRun(t, vault, "bundle", "verify", result.BundlePath)
	if !strings.Contains(gitRun(t, vault, "log", "--format=%s", "refs/mora/backup/run1/local"), "local") {
		t.Fatal("local history not retained")
	}
	if !strings.Contains(gitRun(t, vault, "log", "--format=%s", "refs/mora/backup/run1/remote"), "remote") {
		t.Fatal("remote history not retained")
	}
}

func TestPullSnapshotUsesSelectedTargetAndNeverTouchesLiveVault(t *testing.T) {
	vault, _, opts := backupFixture(t)
	other := cloneOther(t, opts.Target.CanonicalURL)
	writeGitFile(t, other, "remote.md", "remote-only")
	gitRun(t, other, "add", ".")
	gitRun(t, other, "commit", "-m", "remote")
	gitRun(t, other, "push", "origin", "main")
	before := gitRun(t, vault, "status", "--porcelain")
	dest := filepath.Join(opts.StateDir, "backup", "restores", opts.RunID)
	result, err := PullSnapshot(context.Background(), opts, dest)
	if err != nil {
		t.Fatal(err)
	}
	if result.Commit == "" || string(mustRead(t, filepath.Join(result.Destination, "remote.md"))) != "remote-only" {
		t.Fatalf("pull=%#v", result)
	}
	if after := gitRun(t, vault, "status", "--porcelain"); after != before {
		t.Fatalf("pull changed live vault: before=%q after=%q", before, after)
	}
}

func TestPullSnapshotPreservesBinaryBytes(t *testing.T) {
	vault, _, opts := backupFixture(t)
	other := cloneOther(t, opts.Target.CanonicalURL)
	want := []byte{' ', 0, 1, '\n', 'x', '\n', ' '}
	path := filepath.Join(other, "binary.bin")
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, other, "add", ".")
	gitRun(t, other, "commit", "-m", "binary")
	gitRun(t, other, "push", "origin", "main")
	dest := filepath.Join(opts.StateDir, "backup", "restores", opts.RunID)
	if _, err := PullSnapshot(context.Background(), opts, dest); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, filepath.Join(dest, "vault", "binary.bin")); string(got) != string(want) {
		t.Fatalf("binary bytes changed: %v", got)
	}
	if got := gitRun(t, vault, "status", "--porcelain"); got != "" {
		t.Fatalf("live vault changed: %q", got)
	}
}

func TestPullSnapshotRejectsTrackedSensitivePaths(t *testing.T) {
	_, _, opts := backupFixture(t)
	other := cloneOther(t, opts.Target.CanonicalURL)
	writeGitFile(t, other, "tokens/secret", "not-a-real-secret")
	gitRun(t, other, "add", ".")
	gitRun(t, other, "commit", "-m", "unsafe")
	gitRun(t, other, "push", "origin", "main")
	dest := filepath.Join(opts.StateDir, "backup", "restores", opts.RunID)
	if _, err := PullSnapshot(context.Background(), opts, dest); err == nil {
		t.Fatal("sensitive remote tree was accepted")
	}
	if entries, err := os.ReadDir(dest); err == nil && len(entries) != 0 {
		t.Fatalf("unsafe pull exported files: %v", entries)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestSelectedTargetVerificationAndOriginDenyBeforeTransfer(t *testing.T) {
	vault, _, opts := backupFixture(t)
	called := false
	opts.Verify = func(context.Context, SelectedTarget) (VerifiedTarget, error) {
		return VerifiedTarget{}, os.ErrPermission
	}
	opts.run = func(_ context.Context, _ string, _ int, _ ...string) ([]byte, error) {
		called = true
		return nil, nil
	}
	if err := PushSelected(context.Background(), opts); err == nil || called {
		t.Fatalf("verification failure transfer called=%v", called)
	}
	opts = backupOptions(vault, opts.Target.CanonicalURL, filepath.Join(t.TempDir(), "state"))
	gitRun(t, vault, "remote", "set-url", "origin", opts.Target.CanonicalURL+"-wrong")
	if err := PushSelected(context.Background(), opts); err == nil {
		t.Fatal("origin mismatch accepted")
	}
}

func TestProbeRelationRemoteAheadAndNoInitialCommit(t *testing.T) {
	_, _, opts := backupFixture(t)
	other := cloneOther(t, opts.Target.CanonicalURL)
	writeGitFile(t, other, "remote.md", "remote")
	gitRun(t, other, "add", ".")
	gitRun(t, other, "commit", "-m", "remote")
	gitRun(t, other, "push", "origin", "main")
	result, err := ProbeRelation(context.Background(), opts)
	if err != nil || result.Relation != RelationRemoteAhead {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	empty := t.TempDir()
	gitRun(t, empty, "init", "-b", "main")
	gitRun(t, empty, "remote", "add", "origin", opts.Target.CanonicalURL)
	emptyOpts := backupOptions(empty, opts.Target.CanonicalURL, filepath.Join(t.TempDir(), "state"))
	if result, err := ProbeRelation(context.Background(), emptyOpts); err != nil || result.Relation != RelationRemoteAhead || result.LocalOID != "" || result.BundlePath == "" {
		t.Fatalf("remote-only history not preserved: %+v %v", result, err)
	}
}

func backupFixture(t *testing.T) (string, string, BackupOptions) {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	gitRun(t, root, "init", "--bare", remote)
	vault := filepath.Join(root, "vault")
	gitRun(t, root, "init", "-b", "main", vault)
	gitRun(t, vault, "config", "user.email", "test@example.invalid")
	gitRun(t, vault, "config", "user.name", "Test")
	writeGitFile(t, vault, "index.md", "initial")
	gitRun(t, vault, "add", ".")
	gitRun(t, vault, "commit", "-m", "initial")
	url := "file://" + remote
	gitRun(t, vault, "remote", "add", "origin", url)
	gitRun(t, vault, "push", "-u", "origin", "main")
	return vault, remote, backupOptions(vault, url, filepath.Join(root, "state"))
}
func backupOptions(vault, url, state string) BackupOptions {
	target := SelectedTarget{ID: "target1", CanonicalURL: url, RemoteDigest: "digest1", PolicyDigest: "policy1", AuthorizedReaderIDs: []string{"owner", "reader"}}
	return BackupOptions{Vault: vault, StateDir: state, RunID: "run1", RemoteRef: "refs/heads/main", Target: target, testTransport: true, Verify: func(_ context.Context, got SelectedTarget) (VerifiedTarget, error) {
		return VerifiedTarget{ID: got.ID, CanonicalURL: got.CanonicalURL, Private: true, AuthorizedReaderIDs: []string{"reader", "owner"}}, nil
	}}
}
func cloneOther(t *testing.T, url string) string {
	t.Helper()
	other := filepath.Join(t.TempDir(), "other")
	gitRun(t, "", "clone", "--branch", "main", url, other)
	gitRun(t, other, "config", "user.email", "test@example.invalid")
	gitRun(t, other, "config", "user.name", "Test")
	return other
}
func writeGitFile(t *testing.T, root, name, body string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}
