package mora

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestAtomicWriteConcurrentSamePathNotTorn pins the fix for #16: concurrent
// writers to the same target must never produce a torn (interleaved) file.
//
// Each writer writes a homogeneous body — every byte equals its writer id — so
// a complete, atomic write always leaves the file as a single repeated byte.
// With a fixed `<path>.tmp` staging name every writer truncates and writes the
// *same* temp file, so their bytes interleave at the shared inode and the
// renamed result contains a mix of writer ids. The assertion below catches that.
func TestAtomicWriteConcurrentSamePathNotTorn(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "sources.json")

	const writers = 6
	const bodyLen = 128 * 1024 // large enough that interleaved writes tear
	const rounds = 60

	for r := 0; r < rounds; r++ {
		var wg sync.WaitGroup
		for w := 0; w < writers; w++ {
			wg.Add(1)
			go func(id byte) {
				defer wg.Done()
				body := bytes.Repeat([]byte{id}, bodyLen)
				if err := atomicWrite(target, body, 0o600); err != nil {
					t.Errorf("atomicWrite: %v", err)
				}
			}(byte('A' + w))
		}
		wg.Wait()

		got, err := os.ReadFile(target)
		if err != nil {
			t.Fatalf("read target: %v", err)
		}
		if len(got) != bodyLen {
			t.Fatalf("round %d: torn write — length %d, want %d", r, len(got), bodyLen)
		}
		first := got[0]
		for i, b := range got {
			if b != first {
				t.Fatalf("round %d: torn write — byte %d = %q, file starts with %q (interleaved writers)",
					r, i, b, first)
			}
		}
	}
}

// TestAtomicWritePreservesMode guards that the fix still honors the requested
// permission bits. os.CreateTemp starts a temp file at 0600, so the fix must
// Chmod up to the caller's mode before rename.
func TestAtomicWritePreservesMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	if err := atomicWrite(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("atomicWrite: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o644 {
		t.Fatalf("mode = %o, want 0644", got)
	}
}

// TestAtomicWriteNoOrphanTemp verifies a successful write leaves no stray temp
// files in the target directory (only the final file remains).
func TestAtomicWriteNoOrphanTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	if err := atomicWrite(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("atomicWrite: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "f" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("expected only the final file, got %v", names)
	}
}
