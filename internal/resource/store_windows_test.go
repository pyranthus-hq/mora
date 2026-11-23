//go:build windows

package resource

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestResourceWindowsJunctionRefused(t *testing.T) {
	root := t.TempDir()
	r, v, _, err := Import(root, skill("", "# One"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "resources", r.StableID, "revisions", v.RevisionDigest)
	outside := filepath.Join(t.TempDir(), "revision")
	if err := os.Rename(path, outside); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("cmd", "/c", "mklink", "/J", path, outside).CombinedOutput(); err != nil {
		t.Fatalf("junction fixture: %v: %s", err, output)
	}
	if _, _, err := ReadRevision(root, r.StableID, v.RevisionDigest); err == nil {
		t.Fatal("followed junction revision")
	}
	if _, _, _, err := Import(root, skill("", "# One")); err == nil {
		t.Fatal("duplicate followed junction revision")
	}
	if err := plainExistingDir(path); err == nil {
		t.Fatal("accepted selected junction root")
	}
}

func TestResourceWindowsImportedExecutableMetadata(t *testing.T) {
	root := t.TempDir()
	r, v, _, err := Import(root, skill("", "# One"))
	if err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(root, "resources", r.StableID, "revisions", v.RevisionDigest)
	v.Files[0].Executable = true
	v.TreeDigest = digestFiles(v.Files)
	v.RevisionDigest = revisionDigest(v)
	next := filepath.Join(filepath.Dir(old), v.RevisionDigest)
	if err := os.Rename(old, next); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(next, "manifest.json"), mustJSON(v), 0600); err != nil {
		t.Fatal(err)
	}
	_, got, err := ReadRevision(root, r.StableID, v.RevisionDigest)
	if err != nil || !got.Files[0].Executable {
		t.Fatalf("lost portable metadata: %#v %v", got, err)
	}
	if ok, err := storedRevisionMatches(next, v); err != nil || !ok {
		t.Fatalf("portable duplicate: %v %v", ok, err)
	}
	if err := os.WriteFile(filepath.Join(next, "tree", "SKILL.md"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadRevision(root, r.StableID, v.RevisionDigest); err == nil {
		t.Fatal("accepted changed bytes")
	}
}
