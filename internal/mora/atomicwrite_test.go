package mora

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestAtomicWriteConcurrentSamePath verifies that concurrent writers to the
// SAME target never leave a torn/mixed file. With a fixed `path+".tmp"` staging
// name, two writers share one temp file: interleaved truncating writes corrupt
// the staged bytes before the rename, so the surviving file is a byte-wise mix
// of two bodies. A unique temp per writer makes each rename atomic, so the
// final file must always equal exactly one writer's body.
func TestAtomicWriteConcurrentSamePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sources.json")

	const writers = 16
	const bodyLen = 1 << 16 // large enough that writes can interleave at the byte level

	bodies := make([][]byte, writers)
	for i := range bodies {
		bodies[i] = bytes.Repeat([]byte{byte('A' + i)}, bodyLen)
	}

	matchesACandidate := func(got []byte) bool {
		for _, b := range bodies {
			if bytes.Equal(got, b) {
				return true
			}
		}
		return false
	}

	for iter := 0; iter < 200; iter++ {
		var wg sync.WaitGroup
		errs := make([]error, writers)
		for i := 0; i < writers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				errs[i] = atomicWrite(path, bodies[i], 0o600)
			}(i)
		}
		wg.Wait()

		// A unique temp per writer means every concurrent write succeeds and the
		// rename is atomic. A shared temp lets one writer rename the staged file
		// out from under another, so the loser's rename fails — surface that.
		for i, err := range errs {
			if err != nil {
				t.Fatalf("iter %d: writer %d failed (shared-temp collision): %v", iter, i, err)
			}
		}

		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("iter %d: read target: %v", iter, err)
		}
		if !matchesACandidate(got) {
			t.Fatalf("iter %d: target is corrupted: got %d bytes that match no single writer body", iter, len(got))
		}
	}
}

// TestAtomicWriteConcurrentDifferentPaths is the regression guard the issue
// asks for: concurrent writers to distinct paths in the same directory must
// each produce their own correct file (run under -race).
func TestAtomicWriteConcurrentDifferentPaths(t *testing.T) {
	dir := t.TempDir()

	const writers = 16
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			path := filepath.Join(dir, "file"+string(rune('a'+i))+".json")
			body := bytes.Repeat([]byte{byte('a' + i)}, 4096)
			if err := atomicWrite(path, body, 0o600); err != nil {
				t.Errorf("writer %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	for i := 0; i < writers; i++ {
		path := filepath.Join(dir, "file"+string(rune('a'+i))+".json")
		want := bytes.Repeat([]byte{byte('a' + i)}, 4096)
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("writer %d: read back: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("writer %d: content mismatch", i)
		}
	}
}

// TestAtomicWriteHonorsModeAndLeavesNoOrphan verifies the written file has the
// requested permissions and that no staging temp file is left behind on success.
func TestAtomicWriteHonorsModeAndLeavesNoOrphan(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "perm.json")

	// CreateTemp starts at 0600, so 0644 proves atomicWrite applies the caller's
	// requested mode before publishing the file.
	if err := atomicWrite(path, []byte("body\n"), 0o644); err != nil {
		t.Fatalf("atomicWrite: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Fatalf("mode = %o, want 0644", got)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "perm.json" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("expected only perm.json, found %v (orphan temp left behind)", names)
	}
}
