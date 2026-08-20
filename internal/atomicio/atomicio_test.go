package atomicio

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// assertPermUnix asserts an exact Unix permission bit set, but only off Windows.
// Windows has no Unix mode bits: os.FileInfo.Mode().Perm() is synthesized from
// ACLs and reports 0666 for any writable file (0444 for read-only), so it can
// never equal 0600/0640/0644. The production code still writes the correct mode
// (security-relevant on Unix); this only relaxes the *assertion* on Windows.
func assertPermUnix(t *testing.T, got, want os.FileMode) {
	t.Helper()
	if runtime.GOOS != "windows" && got.Perm() != want.Perm() {
		t.Fatalf("mode = %v, want %v", got.Perm(), want.Perm())
	}
}

// TestWriteConcurrentSamePath verifies that concurrent writers to the SAME
// target never leave a torn/mixed file. With a fixed `path+".tmp"` staging
// name, two writers share one temp file: interleaved truncating writes corrupt
// the staged bytes before the rename, so the surviving file is a byte-wise mix
// of two bodies. A unique temp per writer makes each rename atomic, so the
// final file must always equal exactly one writer's body.
func TestWriteConcurrentSamePath(t *testing.T) {
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
				errs[i] = Write(path, bodies[i], 0o600)
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

// TestWriteConcurrentDifferentPaths is a regression guard: concurrent writers
// to distinct paths in the same directory must each produce their own correct
// file (run under -race).
func TestWriteConcurrentDifferentPaths(t *testing.T) {
	dir := t.TempDir()

	const writers = 16
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			path := filepath.Join(dir, "file"+string(rune('a'+i))+".json")
			body := bytes.Repeat([]byte{byte('a' + i)}, 4096)
			if err := Write(path, body, 0o600); err != nil {
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

// TestWriteHonorsModeAndLeavesNoOrphan verifies the written file has the
// requested permissions and that no staging temp file is left behind on success.
func TestWriteHonorsModeAndLeavesNoOrphan(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "perm.json")

	// CreateTemp starts at 0600, so 0644 proves Write applies the caller's
	// requested mode before publishing the file.
	if err := Write(path, []byte("body\n"), 0o644); err != nil {
		t.Fatalf("Write: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	assertPermUnix(t, info.Mode(), 0o644)

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

// TestWriteMkdirAllFailure covers the branch where the parent path is a
// regular file, so MkdirAll fails and Write must surface the error rather
// than silently succeed.
func TestWriteMkdirAllFailure(t *testing.T) {
	dir := t.TempDir()
	fileAsDir := filepath.Join(dir, "iamafile")
	if err := os.WriteFile(fileAsDir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Write(filepath.Join(fileAsDir, "child.txt"), []byte("no"), 0o644); err == nil {
		t.Fatal("Write into a path whose parent is a file must error")
	}
}

// TestAppendFile covers appending, parent-dir creation, and the MkdirAll-fail
// branch.
func TestAppendFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "log.jsonl")
	if err := AppendFile(path, "line-a\n"); err != nil {
		t.Fatalf("AppendFile 1: %v", err)
	}
	if err := AppendFile(path, "line-b\n"); err != nil {
		t.Fatalf("AppendFile 2: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "line-a\nline-b\n" {
		t.Fatalf("content = %q, want %q", got, "line-a\nline-b\n")
	}

	fileAsDir := filepath.Join(dir, "notadir")
	if err := os.WriteFile(fileAsDir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := AppendFile(filepath.Join(fileAsDir, "child.txt"), "no"); err == nil {
		t.Fatal("AppendFile into a path whose parent is a file must error")
	}
}

// withMarkerTrace wraps the real durability barriers and appends an event to a
// shared trace so a test can assert the ORDER of fsync / rename / dir-sync.
func withMarkerTrace(t *testing.T) *[]string {
	t.Helper()
	trace := &[]string{}
	origMarker, origDir := MarkerSyncFn, SyncDirFn
	MarkerSyncFn = func(f *os.File) error { *trace = append(*trace, "fsync"); return origMarker(f) }
	SyncDirFn = func(dir string) error { *trace = append(*trace, "dirsync"); return origDir(dir) }
	t.Cleanup(func() { MarkerSyncFn, SyncDirFn = origMarker, origDir })
	return trace
}

// TestDurableMarkerFsyncsBeforeRename (Gate 2 matrix row 33) — the temp file's
// f.Sync fires BEFORE the rename. MUTATION: delete the MarkerSyncFn(f) call in
// WriteDurable => no "fsync" event => RED.
func TestDurableMarkerFsyncsBeforeRename(t *testing.T) {
	dir := t.TempDir()
	trace := withMarkerTrace(t)
	if err := WriteDurable(filepath.Join(dir, "marker.json"), []byte(`{"x":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(*trace, ",")
	if !strings.Contains(got, "fsync") {
		t.Fatalf("no data fsync in the durable-write trace %q", got)
	}
	// fsync must be the FIRST event (before the rename, which precedes dirsync).
	if (*trace)[0] != "fsync" {
		t.Fatalf("trace = %q, want fsync first (data synced before the rename)", got)
	}
}

// TestDurableMarkerSyncsDirBeforeReturn (Gate 2 matrix row 33b) — the
// parent-dir sync fires before WriteDurable returns (i.e. before any caller
// publish may begin). MUTATION: delete the SyncDirFn(dir) call => no "dirsync"
// event => RED.
func TestDurableMarkerSyncsDirBeforeReturn(t *testing.T) {
	dir := t.TempDir()
	trace := withMarkerTrace(t)
	if err := WriteDurable(filepath.Join(dir, "marker.json"), []byte(`{"x":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(*trace, ",")
	if !strings.Contains(got, "dirsync") {
		t.Fatalf("no parent-dir sync in the durable-write trace %q", got)
	}
	// dirsync is the LAST barrier, and it must fire before return: fsync before dirsync.
	if (*trace)[len(*trace)-1] != "dirsync" {
		t.Fatalf("trace = %q, want dirsync last (dir synced before return)", got)
	}
}

// TestSyncDir covers SyncDir directly: syncing a real directory must not
// error. Off Windows, a nonexistent directory must surface os.Open's error;
// on Windows SyncDir is a documented no-op (see sync_windows.go) so that
// branch does not apply.
func TestSyncDir(t *testing.T) {
	if err := SyncDir(t.TempDir()); err != nil {
		t.Fatalf("SyncDir(existing dir): %v", err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	if err := SyncDir(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("SyncDir(nonexistent dir) should error")
	}
}
