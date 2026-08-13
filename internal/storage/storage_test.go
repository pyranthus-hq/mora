package storage

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStatus(t *testing.T) {
	cases := []struct {
		name  string
		bytes int64
		want  string
	}{
		{"empty", 0, "ok"},
		{"at target boundary", TargetBytes, "ok"},
		{"just over target", TargetBytes + 1, "warn"},
		{"at ceiling boundary", CeilingBytes, "warn"},
		{"over ceiling", CeilingBytes + 1, "over"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Status(tc.bytes); got != tc.want {
				t.Fatalf("Status(%d) = %q, want %q", tc.bytes, got, tc.want)
			}
		})
	}
}

func TestDirBytes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.md"), make([]byte, 100), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "b.md"), make([]byte, 250), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := dirBytes(dir); got != 350 {
		t.Fatalf("DirBytes = %d, want 350", got)
	}
	if got := dirBytes(filepath.Join(dir, "missing")); got != 0 {
		t.Fatalf("dirBytes(missing) = %d, want 0", got)
	}
}

func TestVaultBytesNoDoubleCount(t *testing.T) {
	vault := t.TempDir()
	data := filepath.Join(vault, "data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vault, "note.md"), make([]byte, 100), 0o644); err != nil {
		t.Fatal(err)
	}
	index := filepath.Join(data, "index.db")
	if err := os.WriteFile(index, make([]byte, 500), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := vaultBytes(vault, index); got != 600 {
		t.Fatalf("inside-vault index double-counted: got %d, want 600", got)
	}

	vault2, data2 := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(vault2, "note.md"), make([]byte, 100), 0o644); err != nil {
		t.Fatal(err)
	}
	index2 := filepath.Join(data2, "index.db")
	if err := os.WriteFile(index2, make([]byte, 500), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := vaultBytes(vault2, index2); got != 600 {
		t.Fatalf("disjoint layout: got %d, want 600", got)
	}
}

func TestProductBytesDeduplicatesOverlapsAndHardLinks(t *testing.T) {
	root := t.TempDir()
	roots := Roots{DataDir: root, VaultDir: filepath.Join(root, "vault"), ConfigDir: filepath.Join(root, "config"), StateDir: filepath.Join(root, "state")}
	for _, dir := range []string{roots.VaultDir, roots.ConfigDir, roots.StateDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	source := filepath.Join(roots.VaultDir, "body")
	if err := os.WriteFile(source, make([]byte, 31), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(source, filepath.Join(roots.StateDir, "same-body")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	got, err := ProductBytes(roots)
	if err != nil {
		t.Fatal(err)
	}
	if got != 31 {
		t.Fatalf("overlap/hard link charged %d bytes; want 31", got)
	}
}

func TestPathWithinIsCaseSensitive(t *testing.T) {
	if pathWithin(filepath.Join("tmp", "A"), filepath.Join("tmp", "a")) || pathWithin(filepath.Join("tmp", "a"), filepath.Join("tmp", "A")) {
		t.Fatal("case-distinct roots collapsed")
	}
}
