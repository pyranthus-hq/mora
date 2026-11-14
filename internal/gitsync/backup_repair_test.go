package gitsync

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSelectedBackupProductionURLAndPortableIdentity(t *testing.T) {
	for _, url := range []string{"https://github.com/owner/repo.git", "ssh://git@github.com/owner/repo.git", "git@github.com:owner/repo.git"} {
		if !validGitHubURL(url) {
			t.Errorf("valid URL refused: %s", url)
		}
	}
	for _, url := range []string{"https://github.com.evil/owner/repo", "https://github.com/owner/repo?x=1", "https://evil/github.com/owner/repo", "https://github.com/owner/../repo", "git@github.com:owner/repo/extra", "file:///tmp/github.com/repo"} {
		if validGitHubURL(url) {
			t.Errorf("unsafe URL accepted: %s", url)
		}
	}
	for _, id := range []string{".", "..", "CON", "com1.txt", "name.", "x:y"} {
		if safeRun(id) {
			t.Errorf("unsafe run accepted: %s", id)
		}
	}
	for _, path := range []string{"nested/tokens/key", "nested/.env", "nested/.git/config", "nested/Identity-private", "AUTH.JSON", "x.pem"} {
		if !sensitivePath(path) {
			t.Errorf("sensitive path accepted: %s", path)
		}
	}
	seen := map[string]string{}
	if !safeTreePath("Notes/a.md", seen) || safeTreePath("notes/b.md", seen) {
		t.Fatal("case alias accepted")
	}
}

func TestSelectedPushRefusesPushURLBeforeStaging(t *testing.T) {
	vault, _, opts := backupFixture(t)
	other := filepath.Join(t.TempDir(), "other.git")
	gitRun(t, filepath.Dir(other), "init", "--bare", other)
	gitRun(t, vault, "remote", "set-url", "--push", "origin", "file://"+other)
	writeGitFile(t, vault, "uncommitted.md", "preserve")
	before := gitRun(t, vault, "status", "--porcelain")
	if err := PushSelected(context.Background(), opts); err == nil {
		t.Fatal("unverified push URL accepted")
	}
	if after := gitRun(t, vault, "status", "--porcelain"); after != before {
		t.Fatal("denial staged data")
	}
}

func TestSelectedPushDisablesHooksAndContentFilters(t *testing.T) {
	vault, _, opts := backupFixture(t)
	for _, name := range []string{"pre-commit", "pre-push"} {
		if err := os.WriteFile(filepath.Join(vault, ".git", "hooks", name), []byte("#!/bin/sh\nexit 51\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, vault, "config", "filter.reject.clean", "exit 52")
	gitRun(t, vault, "config", "filter.reject.required", "true")
	writeGitFile(t, vault, ".gitattributes", "*.md filter=reject\n")
	writeGitFile(t, vault, "untouched.md", "  exact backup bytes\n")
	if err := PushSelected(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	out, err := selectedCommand(context.Background(), opts, 100, "show", "HEAD:untouched.md")
	if err != nil || string(out) != "  exact backup bytes\n" {
		t.Fatalf("filter altered data: %q %v", out, err)
	}
}

func TestSelectedCommandBoundsOutputAndIgnoresGitEnvironment(t *testing.T) {
	vault, _, opts := backupFixture(t)
	gitRun(t, vault, "config", "fixture.large", "123456789")
	if _, err := selectedCommand(context.Background(), opts, 4, "config", "--get", "fixture.large"); err == nil {
		t.Fatal("unbounded output")
	}
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "wrong"))
	if out, err := selectedCommand(context.Background(), opts, 4096, "rev-parse", "--show-toplevel"); err != nil || !strings.Contains(string(out), filepath.Base(vault)) {
		t.Fatalf("environment redirected Git: %q %v", out, err)
	}
}

func TestPullSnapshotPreservesUserManifestAndRefusesExistingDestination(t *testing.T) {
	vault, _, opts := backupFixture(t)
	writeGitFile(t, vault, "manifest.json", "user content\n")
	gitRun(t, vault, "add", ".")
	gitRun(t, vault, "commit", "-m", "manifest")
	gitRun(t, vault, "push", "origin", "main")
	dest := filepath.Join(opts.StateDir, "backup", "restores", opts.RunID)
	result, err := PullSnapshot(context.Background(), opts, dest)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Published || string(mustRead(t, filepath.Join(result.Destination, "manifest.json"))) != "user content\n" || result.ManifestPath == filepath.Join(result.Destination, "manifest.json") {
		t.Fatalf("manifest overwritten: %+v", result)
	}
	if _, err := PullSnapshot(context.Background(), opts, dest); err == nil {
		t.Fatal("existing snapshot replaced")
	}
}

func TestSelectedBackupRejectsLinkedStateAndForeignDestination(t *testing.T) {
	_, _, opts := backupFixture(t)
	foreign := t.TempDir()
	link := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(foreign, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	opts.StateDir = filepath.Join(link, "state")
	if _, err := PullSnapshot(context.Background(), opts, filepath.Join(opts.StateDir, "backup", "restores", opts.RunID)); err == nil {
		t.Fatal("linked state accepted")
	}
	if entries, err := os.ReadDir(foreign); err != nil || len(entries) != 0 {
		t.Fatal("linked foreign state changed", err)
	}
}

func TestUnrelatedHistoriesRecoverFromBundleAlone(t *testing.T) {
	_, remote, opts := backupFixture(t)
	other := t.TempDir()
	gitRun(t, other, "init", "-b", "main")
	gitRun(t, other, "config", "user.email", "test@example.invalid")
	gitRun(t, other, "config", "user.name", "Test")
	writeGitFile(t, other, "unrelated.md", "unrelated")
	gitRun(t, other, "add", ".")
	gitRun(t, other, "commit", "-m", "unrelated")
	gitRun(t, other, "push", "--force", "file://"+remote, "HEAD:refs/heads/main")
	result, err := ProbeRelation(context.Background(), opts)
	if err != nil || result.Relation != RelationDiverged {
		t.Fatalf("unrelated: %+v %v", result, err)
	}
	restored := filepath.Join(t.TempDir(), "recovered.git")
	gitRun(t, filepath.Dir(restored), "init", "--bare", restored)
	gitRun(t, restored, "fetch", result.BundlePath, "refs/mora/backup/run1/local:refs/heads/local", "refs/mora/backup/run1/remote:refs/heads/remote")
	for _, oid := range []string{result.LocalOID, result.RemoteOID} {
		gitRun(t, restored, "cat-file", "-e", oid+"^{commit}")
	}
}

func TestSelectedBackupRejectsLinkedReconcileLeaf(t *testing.T) {
	_, _, opts := backupFixture(t)
	foreign := t.TempDir()
	root := filepath.Join(opts.StateDir, "backup", "reconcile", opts.RunID)
	if err := os.MkdirAll(filepath.Dir(root), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(foreign, root); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := preserveRelation(context.Background(), opts, &RelationResult{}); err == nil {
		t.Fatal("linked reconcile leaf accepted")
	}
	if entries, err := os.ReadDir(foreign); err != nil || len(entries) != 0 {
		t.Fatal("foreign directory changed", err)
	}
}

func TestSelectedBackupDoesNotReuseOperationRef(t *testing.T) {
	_, _, opts := backupFixture(t)
	if _, err := ProbeRelation(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	ref := "refs/mora/backup/" + opts.RunID + "/remote"
	before := gitRun(t, opts.Vault, "rev-parse", ref)
	if _, err := ProbeRelation(context.Background(), opts); err == nil {
		t.Fatal("operation ref reused")
	}
	if after := gitRun(t, opts.Vault, "rev-parse", ref); before != after {
		t.Fatal("preserved ref changed")
	}
}
