package managedtree

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
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
}

func TestInstallTreeRecoversInterruptedSwap(t *testing.T) {
	root := t.TempDir()
	source1 := filepath.Join(root, "source1")
	source2 := filepath.Join(root, "source2")
	destination := filepath.Join(root, "destination")
	writeTree(t, source1, map[string]string{"managed.txt": "one"})
	writeTree(t, source2, map[string]string{"managed.txt": "two"})
	first, _ := DigestTree(source1)
	manifest, err := InstallTree(first, destination, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := DigestTree(source2)
	_, parent, base, err := destinationParts(destination)
	if err != nil {
		t.Fatal(err)
	}
	stage, backup := stageNames(base)
	parentRoot, err := os.OpenRoot(parent)
	if err != nil {
		t.Fatal(err)
	}
	if err := parentRoot.Mkdir(stage, 0o700); err != nil {
		t.Fatal(err)
	}
	stageRoot, err := parentRoot.OpenRoot(stage)
	if err != nil {
		t.Fatal(err)
	}
	sourceRoot, err := os.OpenRoot(second.Source)
	if err != nil {
		t.Fatal(err)
	}
	if err := copyFiles(sourceRoot, stageRoot, second.Files); err != nil {
		t.Fatal(err)
	}
	_ = sourceRoot.Close()
	_ = stageRoot.Close()
	if err := parentRoot.Rename(base, backup); err != nil {
		t.Fatal(err)
	}
	_ = parentRoot.Close()
	if _, err := InstallTree(second, destination, &manifest); err != nil {
		t.Fatalf("recovery install: %v", err)
	}
	assertFile(t, filepath.Join(destination, "managed.txt"), "two")
	if _, err := os.Lstat(filepath.Join(parent, backup)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("backup remains after recovery: %v", err)
	}
}

func TestRejectsHostileNamesAndLinks(t *testing.T) {
	for _, path := range []string{"../escape", "/absolute", "dir\\escape", "CON", "name.", "a/../b"} {
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
