//go:build darwin

package resource

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResourceMacTemporaryAncestorsPreserveLinkBoundary(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "mora-resource-alias-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	r, v, _, err := Import(root, skill("", "# Temporary fixture"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadRevision(root, r.StableID, v.RevisionDigest); err != nil {
		t.Fatal(err)
	}
	if err := plainExistingDir("/tmp"); err == nil {
		t.Fatal("selected root symlink was accepted")
	}
	link := filepath.Join(root, "arbitrary-alias")
	outside := t.TempDir()
	if err := os.Mkdir(filepath.Join(outside, "child"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{link, filepath.Join(link, "child")} {
		if err := plainExistingDir(path); err == nil {
			t.Fatalf("accepted uncontrolled alias: %s", path)
		}
	}
}
