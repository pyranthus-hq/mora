package managedtree

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestDigestTreeBindsPathsContentAndExecutableBit(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"bin/run": "echo hello\n", "notes/a.txt": "alpha"})
	if err := os.Chmod(filepath.Join(root, "bin/run"), 0o755); err != nil {
		t.Fatal(err)
	}
	first, err := DigestTree(root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := DigestTree(root)
	if err != nil || first.Digest != second.Digest || !reflect.DeepEqual(first.Files, second.Files) {
		t.Fatalf("digest is not stable: %#v %#v %v", first, second, err)
	}
	if err := os.Chmod(filepath.Join(root, "bin/run"), 0o644); err != nil {
		t.Fatal(err)
	}
	modeChanged, err := DigestTree(root)
	if err != nil || modeChanged.Digest == first.Digest {
		t.Fatalf("executable-bit change did not alter digest: %v", err)
	}
	if err := os.Rename(filepath.Join(root, "notes/a.txt"), filepath.Join(root, "notes/b.txt")); err != nil {
		t.Fatal(err)
	}
	pathChanged, err := DigestTree(root)
	if err != nil || pathChanged.Digest == modeChanged.Digest {
		t.Fatalf("path change did not alter digest: %v", err)
	}
}

func TestInstallTreeUpdatesManagedFilesAndPreservesForeignFiles(t *testing.T) {
	root := t.TempDir()
	source1 := filepath.Join(root, "source1")
	source2 := filepath.Join(root, "source2")
	destination := filepath.Join(root, "destination")
	writeTree(t, source1, map[string]string{"a.txt": "one", "old.txt": "old"})
	first, err := DigestTree(source1)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := InstallTree(first, destination, nil)
	if err != nil {
		t.Fatal(err)
	}
	writeTree(t, destination, map[string]string{"foreign/keep.txt": "foreign"})
	if err := os.Chmod(filepath.Join(destination, "foreign/keep.txt"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeTree(t, source2, map[string]string{"a.txt": "two", "new.txt": "new"})
	second, err := DigestTree(source2)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err = InstallTree(second, destination, &manifest)
	if err != nil {
		t.Fatal(err)
	}
	assertFile(t, filepath.Join(destination, "a.txt"), "two")
	assertFile(t, filepath.Join(destination, "new.txt"), "new")
	assertFile(t, filepath.Join(destination, "foreign/keep.txt"), "foreign")
	if info, err := os.Stat(filepath.Join(destination, "foreign/keep.txt")); err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("foreign mode = %v, %v; want 0644", info.Mode(), err)
	}
	if _, err := os.Lstat(filepath.Join(destination, "old.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old managed file remains: %v", err)
	}
	if manifest.Revision.Digest != second.Digest {
		t.Fatalf("manifest digest = %s, want %s", manifest.Revision.Digest, second.Digest)
	}
}

func TestInstallTreeRefusesEditedManagedAndUnmanagedDestination(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	destination := filepath.Join(root, "destination")
	writeTree(t, source, map[string]string{"managed.txt": "original"})
	revision, err := DigestTree(source)
	if err != nil {
		t.Fatal(err)
	}
	writeTree(t, destination, map[string]string{"foreign.txt": "foreign"})
	if _, err := InstallTree(revision, destination, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("unmanaged destination error = %v, want conflict", err)
	}
	if err := os.RemoveAll(destination); err != nil {
		t.Fatal(err)
	}
	manifest, err := InstallTree(revision, destination, nil)
	if err != nil {
		t.Fatal(err)
	}
	writeTree(t, destination, map[string]string{"managed.txt": "local edit"})
	if _, err := InstallTree(revision, destination, &manifest); !errors.Is(err, ErrConflict) {
		t.Fatalf("edited managed file error = %v, want conflict", err)
	}
	assertFile(t, filepath.Join(destination, "managed.txt"), "local edit")
}

func TestDetachTreePreservesEditedAndForeignFiles(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	destination := filepath.Join(root, "destination")
	writeTree(t, source, map[string]string{"keep.txt": "keep", "remove.txt": "remove"})
	revision, err := DigestTree(source)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := InstallTree(revision, destination, nil)
	if err != nil {
		t.Fatal(err)
	}
	writeTree(t, destination, map[string]string{"keep.txt": "edited", "foreign.txt": "foreign"})
	removed, err := DetachTree(destination, manifest)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("detach error = %v, want conflict", err)
	}
	if !reflect.DeepEqual(removed, []string{"remove.txt"}) {
		t.Fatalf("removed = %#v", removed)
	}
	assertFile(t, filepath.Join(destination, "keep.txt"), "edited")
	assertFile(t, filepath.Join(destination, "foreign.txt"), "foreign")
	if removed, err := DetachTree(destination, manifest); !errors.Is(err, ErrConflict) || len(removed) != 0 {
		t.Fatalf("detach retry = %#v, %v; want no duplicate removals and conflict", removed, err)
	}
}

func TestInstallTreePreservesIncompleteTransaction(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	destination := filepath.Join(root, "destination")
	writeTree(t, source, map[string]string{"managed.txt": "approved"})
	writeTree(t, destination, map[string]string{"local.txt": "keep"})
	revision, err := DigestTree(source)
	if err != nil {
		t.Fatal(err)
	}
	_, parent, base, err := destinationParts(destination)
	if err != nil {
		t.Fatal(err)
	}
	stage := ".destination.managedtree-stage-interrupted"
	backup := stage + ".backup"
	if err := os.Mkdir(filepath.Join(parent, stage), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTree(t, filepath.Join(parent, stage), map[string]string{"partial": "stage"})
	if err := beginTransaction(parent, transactionMarker(base), stage, backup); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallTree(revision, destination, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("incomplete transaction error = %v, want conflict", err)
	}
	assertFile(t, filepath.Join(destination, "local.txt"), "keep")
	assertFile(t, filepath.Join(parent, stage, "partial"), "stage")
}

func TestIncompleteTransactionBlocksDifferentRevisionWithoutDestination(t *testing.T) {
	root := t.TempDir()
	firstSource, secondSource := filepath.Join(root, "first"), filepath.Join(root, "second")
	destination := filepath.Join(root, "destination")
	writeTree(t, firstSource, map[string]string{"managed": "first"})
	writeTree(t, secondSource, map[string]string{"managed": "second"})
	second, err := DigestTree(secondSource)
	if err != nil {
		t.Fatal(err)
	}
	_, parent, base, err := destinationParts(destination)
	if err != nil {
		t.Fatal(err)
	}
	stage := ".destination.managedtree-stage-partial"
	if err := os.Mkdir(filepath.Join(parent, stage), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTree(t, filepath.Join(parent, stage), map[string]string{"managed": "first"})
	if err := beginTransaction(parent, transactionMarker(base), stage, stage+".backup"); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallTree(second, destination, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("different revision with partial transaction = %v, want conflict", err)
	}
	assertFile(t, filepath.Join(parent, stage, "managed"), "first")
	if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing destination was changed: %v", err)
	}
}

func TestInstallTreeIgnoresUnownedLegacySidecars(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	destination := filepath.Join(root, "skill")
	backup := filepath.Join(root, ".skill.managedtree-backup")
	writeTree(t, source, map[string]string{"SKILL.md": "same approved text"})
	writeTree(t, destination, map[string]string{"SKILL.md": "same approved text"})
	writeTree(t, backup, map[string]string{"personal.txt": "unmanaged"})
	revision, err := DigestTree(source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := InstallTree(revision, destination, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("unmanaged destination error = %v, want conflict", err)
	}
	assertFile(t, filepath.Join(backup, "personal.txt"), "unmanaged")
}

func TestSyncFailurePreservesPriorDestination(t *testing.T) {
	root := t.TempDir()
	oldSource, newSource := filepath.Join(root, "old"), filepath.Join(root, "new")
	destination := filepath.Join(root, "destination")
	writeTree(t, oldSource, map[string]string{"managed": "old"})
	writeTree(t, newSource, map[string]string{"managed": "new"})
	oldRevision, _ := DigestTree(oldSource)
	manifest, err := InstallTree(oldRevision, destination, nil)
	if err != nil {
		t.Fatal(err)
	}
	newRevision, _ := DigestTree(newSource)
	originalSync := syncFile
	syncFile = func(*os.File) error { return errors.New("file sync failure") }
	t.Cleanup(func() { syncFile = originalSync })
	if _, err := InstallTree(newRevision, destination, &manifest); err == nil {
		t.Fatal("install succeeded despite staged file sync failure")
	}
	assertFile(t, filepath.Join(destination, "managed"), "old")
}

func TestPostParkSyncFailurePreservesTransactionPaths(t *testing.T) {
	root := t.TempDir()
	oldSource, newSource := filepath.Join(root, "old"), filepath.Join(root, "new")
	destination := filepath.Join(root, "destination")
	writeTree(t, oldSource, map[string]string{"managed": "old"})
	writeTree(t, newSource, map[string]string{"managed": "new"})
	oldRevision, _ := DigestTree(oldSource)
	manifest, err := InstallTree(oldRevision, destination, nil)
	if err != nil {
		t.Fatal(err)
	}
	newRevision, _ := DigestTree(newSource)
	originalSync := syncDir
	parentCalls := 0
	syncDir = func(path string) error {
		if path == root {
			parentCalls++
			if parentCalls == 2 { // marker durable, then old destination parked
				return errors.New("parent sync failure")
			}
		}
		return originalSync(path)
	}
	t.Cleanup(func() { syncDir = originalSync })
	if _, err := InstallTree(newRevision, destination, &manifest); !errors.Is(err, ErrConflict) {
		t.Fatalf("post-park sync error = %v, want recoverable conflict", err)
	}
	if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destination should remain unclaimed after failed park sync: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, transactionMarker("destination"))); err != nil {
		t.Fatalf("transaction marker missing: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".backup") {
			assertFile(t, filepath.Join(root, entry.Name(), "managed"), "old")
			return
		}
	}
	t.Fatal("preserved backup was not found")
}

func TestPostPublishSyncFailurePreservesNewAndPriorTrees(t *testing.T) {
	root := t.TempDir()
	oldSource, newSource := filepath.Join(root, "old"), filepath.Join(root, "new")
	destination := filepath.Join(root, "destination")
	writeTree(t, oldSource, map[string]string{"managed": "old"})
	writeTree(t, newSource, map[string]string{"managed": "new"})
	oldRevision, _ := DigestTree(oldSource)
	manifest, err := InstallTree(oldRevision, destination, nil)
	if err != nil {
		t.Fatal(err)
	}
	newRevision, _ := DigestTree(newSource)
	originalSync := syncDir
	parentCalls := 0
	syncDir = func(path string) error {
		if path == root {
			parentCalls++
			if parentCalls == 3 { // old parked, staged tree published, then barrier fails
				return errors.New("published parent sync failure")
			}
		}
		return originalSync(path)
	}
	t.Cleanup(func() { syncDir = originalSync })
	if _, err := InstallTree(newRevision, destination, &manifest); !errors.Is(err, ErrConflict) {
		t.Fatalf("post-publish sync error = %v, want recoverable conflict", err)
	}
	assertFile(t, filepath.Join(destination, "managed"), "new")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".backup") {
			assertFile(t, filepath.Join(root, entry.Name(), "managed"), "old")
			return
		}
	}
	t.Fatal("prior backup was not preserved after post-publish failure")
}

func TestRejectsHostileNamesAndLinks(t *testing.T) {
	for _, path := range []string{"../escape", "/absolute", "dir\\escape", "CON", "name.", "a/../b", "nul\x00name", "line\nbreak"} {
		if err := validRelative(path, map[string]string{}); err == nil {
			t.Fatalf("accepted hostile path %q", path)
		}
	}
	seen := map[string]string{}
	if err := validRelative("Foo/a", seen); err != nil {
		t.Fatal(err)
	}
	if err := validRelative("foo/b", seen); err == nil {
		t.Fatal("accepted case-fold platform alias")
	}
	if !unsafeMode(os.ModeIrregular) {
		t.Fatal("irregular files must be rejected")
	}
	root := t.TempDir()
	outside := filepath.Join(root, "outside")
	writeTree(t, outside, map[string]string{"secret": "nope"})
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(source, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := DigestTree(source); !errors.Is(err, ErrConflict) {
		t.Fatalf("link digest error = %v, want conflict", err)
	}
}

func TestInstallTreeRejectsDestinationLink(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	outside := filepath.Join(root, "outside")
	writeTree(t, source, map[string]string{"file": "contents"})
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "destination")
	if err := os.Symlink(outside, destination); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	revision, err := DigestTree(source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := InstallTree(revision, destination, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("destination link error = %v, want conflict", err)
	}
	if _, err := os.Lstat(filepath.Join(outside, "file")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destination escape wrote outside: %v", err)
	}
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for path, contents := range files {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != want {
		t.Fatalf("%s = %q, %v; want %q", path, got, err, want)
	}
}

func TestFilesSorted(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"z": "z", "a": "a"})
	revision, err := DigestTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if !sort.SliceIsSorted(revision.Files, func(i, j int) bool { return revision.Files[i].Path < revision.Files[j].Path }) {
		t.Fatal("digest files are not sorted")
	}
}
