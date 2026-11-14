//go:build windows

package gitsync

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestSelectedBackupRejectsWindowsJunctionLeaf(t *testing.T) {
	root, foreign := t.TempDir(), t.TempDir()
	link := filepath.Join(root, "junction")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, foreign).CombinedOutput(); err != nil {
		t.Fatalf("junction fixture: %v %s", err, out)
	}
	defer os.Remove(link)
	if safeOwnedPath(root, link) || safeBackupDirectory(filepath.Join(link, "child")) == nil {
		t.Fatal("junction accepted")
	}
	if entries, err := os.ReadDir(foreign); err != nil || len(entries) != 0 {
		t.Fatal("foreign directory changed", err)
	}
}
