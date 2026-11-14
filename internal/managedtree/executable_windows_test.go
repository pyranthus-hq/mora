//go:build windows

package managedtree

import (
	"path/filepath"
	"testing"
)

func TestWindowsAcceptsImportedExecutableManifest(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	destination := filepath.Join(root, "destination")
	writeTree(t, source, map[string]string{"run": "script bytes"})
	revision, err := DigestTree(source)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate an approved macOS revision: bytes and paths are identical, but
	// Windows cannot reproduce its POSIX executable bit.
	revision.Files[0].Executable = true
	revision.Digest = digest(revision.Files)
	manifest, err := InstallTree(revision, destination, nil)
	if err != nil {
		t.Fatalf("install imported executable manifest: %v", err)
	}
	if _, err := DetachTree(destination, manifest); err != nil {
		t.Fatalf("detach imported executable manifest: %v", err)
	}
}
