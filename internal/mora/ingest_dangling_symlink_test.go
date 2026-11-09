//go:build !windows

package mora

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pyranthus-hq/mora/internal/memory"
)

func danglingWrite(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("Retained filesystem evidence."), 0600); err != nil {
		t.Fatal(err)
	}
}

func danglingMemory(cfg Config, s Source, rel string) string {
	return filepath.Join(sourcesRoot(cfg), s.Type, s.Name, "src_"+ContentHash(s.Name+":"+rel)+".md")
}

func TestDanglingSymlinkContinuesCleanupAndSave(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "target.md")
	danglingWrite(t, target)
	link := filepath.Join(root, "a-link.md")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"b.md", "keep.md"} {
		danglingWrite(t, filepath.Join(root, name))
	}
	s := Source{Name: "dangling", Type: "filesystem", Path: root, Scope: "project:mora"}
	if _, err := ingestFilesystemDetailed(context.Background(), cfg, s, io.Discard); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(danglingMemory(cfg, s, "a-link.md"))
	if err != nil {
		t.Fatal(err)
	}
	// AC-002.2 Pin a distinct prior success so the check cannot pass by clock coincidence.
	statusPath := syncStatusPathFor(cfg, s)
	status, err := memory.LoadStatus(statusPath)
	if err != nil {
		t.Fatal(err)
	}
	status.LastSuccessAt = "2000-01-01T00:00:00Z"
	status.LastSynced = status.LastSuccessAt
	if err := memory.SaveStatus(statusPath, status); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{target, filepath.Join(root, "b.md")} {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	danglingWrite(t, filepath.Join(root, "c.md"))
	result, err := ingestFilesystemDetailed(context.Background(), cfg, s, io.Discard)
	// AC-002.1
	if err == nil || !errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), "symlink target") || !strings.Contains(err.Error(), link) {
		t.Fatalf("missing dangling target error: %v", err)
	}
	// AC-001.1
	if _, err := os.Stat(danglingMemory(cfg, s, "b.md")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("deleted memory remains: %v", err)
	}
	// AC-001.2
	if _, err := os.Stat(danglingMemory(cfg, s, "c.md")); err != nil {
		t.Fatal(err)
	}
	// AC-001.3
	manifest := loadFilesystemManifest(filesystemManifestPath(cfg, s.Name))
	for _, name := range []string{"a-link.md", "c.md", "keep.md"} {
		if _, ok := manifest[name]; !ok {
			t.Errorf("manifest missing %s", name)
		}
	}
	if _, ok := manifest["b.md"]; ok {
		t.Fatal("manifest retained b.md")
	}
	// AC-001.4
	after, err := os.ReadFile(danglingMemory(cfg, s, "a-link.md"))
	if err != nil || string(after) != string(before) {
		t.Fatalf("link memory changed: %v", err)
	}
	if _, err := os.Stat(danglingMemory(cfg, s, "keep.md")); err != nil {
		t.Fatal(err)
	}
	if result.Unchanged != 1 {
		t.Fatalf("unchanged=%d, want only keep.md", result.Unchanged)
	}
	// AC-002.2
	afterStatus, err := memory.LoadStatus(statusPath)
	if err != nil {
		t.Fatal(err)
	}
	if afterStatus.LastError == "" || afterStatus.ErrorCount != 1 || afterStatus.LastSuccessAt != status.LastSuccessAt || afterStatus.LastSynced != status.LastSynced {
		t.Fatalf("incorrect failure status: %+v", afterStatus)
	}
}

func TestDanglingSymlinkWithoutPriorEntry(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	root := t.TempDir()
	s := Source{Name: "new-dangling", Type: "filesystem", Path: root, Scope: "project:mora"}
	// AC-001.5 Multiple missing targets also exercise error joining.
	for _, name := range []string{"a-link.md", "b-link.md"} {
		if err := os.Symlink(filepath.Join(root, "missing"), filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	danglingWrite(t, filepath.Join(root, "c.md"))
	result, err := ingestFilesystemDetailed(context.Background(), cfg, s, io.Discard)
	if err == nil || !errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), "a-link.md") || !strings.Contains(err.Error(), "b-link.md") {
		t.Fatalf("missing joined errors: %v", err)
	}
	manifest := loadFilesystemManifest(filesystemManifestPath(cfg, s.Name))
	if len(manifest) != 1 {
		t.Fatalf("unexpected manifest: %+v", manifest)
	}
	if _, ok := manifest["c.md"]; !ok {
		t.Fatal("c.md not saved")
	}
	if result.Materialized != 1 {
		t.Fatalf("unexpected writes: %+v", result)
	}
	for _, name := range []string{"a-link.md", "b-link.md"} {
		if _, err := os.Stat(danglingMemory(cfg, s, name)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("unexpected link memory: %v", err)
		}
	}
	if _, err := os.Stat(danglingMemory(cfg, s, "c.md")); err != nil {
		t.Fatal(err)
	}
}

func TestDanglingSymlinkFatalTargetsAbort(t *testing.T) {
	for _, kind := range []string{"directory", "loop"} {
		t.Run(kind, func(t *testing.T) {
			cfg := coreBIngestInitCfg(t)
			root := t.TempDir()
			s := Source{Name: "fatal-link", Type: "filesystem", Path: root, Scope: "project:mora"}
			danglingWrite(t, filepath.Join(root, "b.md"))
			if _, err := ingestFilesystemDetailed(context.Background(), cfg, s, io.Discard); err != nil {
				t.Fatal(err)
			}
			manifestPath := filesystemManifestPath(cfg, s.Name)
			before, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(root, "b.md")); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(root, "a-link.md")
			target := link
			if kind == "directory" {
				target = t.TempDir()
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			danglingWrite(t, filepath.Join(root, "c.md"))
			_, err = ingestFilesystemDetailed(context.Background(), cfg, s, io.Discard)
			// AC-002.3 Directory targets still abort before cleanup.
			if kind == "directory" && (err == nil || !strings.Contains(err.Error(), "is not a regular file")) {
				t.Fatalf("directory error: %v", err)
			}
			// AC-002.4 A self-referencing link produces a non-not-exist Stat error, even as root.
			if kind == "loop" && (err == nil || errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), "symlink target")) {
				t.Fatalf("loop error: %v", err)
			}
			after, readErr := os.ReadFile(manifestPath)
			if readErr != nil || string(after) != string(before) {
				t.Fatalf("fatal error saved manifest: %v", readErr)
			}
			if _, err := os.Stat(danglingMemory(cfg, s, "b.md")); err != nil {
				t.Fatalf("fatal error ran cleanup: %v", err)
			}
			if _, err := os.Stat(danglingMemory(cfg, s, "c.md")); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("walk continued after fatal error: %v", err)
			}
		})
	}
}
