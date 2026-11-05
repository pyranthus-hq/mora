package mora

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFilesystemSymlinkTargetCorrectionReachesScopedSearch(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	root, native := t.TempDir(), filepath.Join(t.TempDir(), "decision.md")
	before, corrected := "Use amberold for this project.", "Use ambernew for this project."
	if err := os.WriteFile(native, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "decision.md")
	if err := os.Symlink(native, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "survivor.md"), []byte("Unrelated surviving note."), 0o600); err != nil {
		t.Fatal(err)
	}
	source := Source{Name: "claude-project-memory", Type: "filesystem", Path: root, Scope: "project:mora"}
	ctx := context.Background()
	ingest := func() sourceIngestResult {
		t.Helper()
		result, err := ingestFilesystemDetailed(ctx, cfg, source, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := rebuildIndex(ctx, cfg); err != nil {
			t.Fatal(err)
		}
		return result
	}
	search := func(query, scope string, want int) []Memory {
		t.Helper()
		rows, err := defaultSearch(ctx, cfg, query, scope, 5)
		if err != nil || len(rows) != want {
			t.Fatalf("search %q in %q: rows=%d want=%d err=%v", query, scope, len(rows), want, err)
		}
		return rows
	}
	ingest()
	search("amberold", source.Scope, 1)
	search("amberold", "project:other", 0)
	linkBefore, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(native, []byte(corrected), 0o600); err != nil {
		t.Fatal(err)
	}
	changed := time.Unix(1800000000, 0)
	if err := os.Chtimes(native, changed, changed); err != nil {
		t.Fatal(err)
	}
	linkAfter, err := os.Lstat(link)
	if err != nil || linkBefore.Size() != linkAfter.Size() || !linkBefore.ModTime().Equal(linkAfter.ModTime()) {
		t.Fatalf("fixture unexpectedly changed the staging link: %v", err)
	}
	if result := ingest(); result.Materialized != 1 || result.Unchanged != 1 {
		t.Fatalf("target correction skipped: %+v", result)
	}
	search("amberold", source.Scope, 0)
	rows := search("ambernew", source.Scope, 1)
	if rows[0].Text != corrected {
		t.Fatalf("stale correction body: %q", rows[0].Text)
	}
	search("ambernew", "project:other", 0)
	if result := ingest(); result.Materialized != 0 || result.Unchanged != 2 {
		t.Fatalf("unchanged targets should remain incremental: %+v", result)
	}
	legacy := loadFilesystemManifest(filesystemManifestPath(cfg, source.Name))
	legacy["decision.md"] = filesystemManifestEntry{Size: linkAfter.Size(), ModTime: linkAfter.ModTime().UnixNano()}
	if err := saveFilesystemManifest(filesystemManifestPath(cfg, source.Name), legacy); err != nil {
		t.Fatal(err)
	}
	if result := ingest(); result.Materialized != 1 {
		t.Fatalf("legacy symlink cache did not refresh once: %+v", result)
	}
	retarget := filepath.Join(t.TempDir(), "decision.md")
	if err := os.WriteFile(retarget, []byte("Use amberalt for this project."), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(retarget, changed, changed); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(retarget, link); err != nil {
		t.Fatal(err)
	}
	ingest()
	search("ambernew", source.Scope, 0)
	search("amberalt", source.Scope, 1)
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	ingest()
	search("amberalt", source.Scope, 0)
}

func TestFilesystemSymlinkDirectoryIsNotTraversed(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	root, target := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "hidden.md"), []byte("Do not discover this directory."), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "directory.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	source := Source{Name: "claude-project-memory", Type: "filesystem", Path: root, Scope: "project:mora"}
	if _, err := ingestFilesystemDetailed(context.Background(), cfg, source, io.Discard); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("directory target was not refused: %v", err)
	}
	files, err := allMemoryFiles(cfg)
	if err != nil || len(files) != 0 {
		t.Fatalf("directory target was traversed: %v, %v", files, err)
	}
}

func TestFilesystemBrokenSymlinkDoesNotReportUnchangedOrPrune(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	root, native := t.TempDir(), filepath.Join(t.TempDir(), "decision.md")
	if err := os.WriteFile(native, []byte("A retained source decision."), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(native, filepath.Join(root, "decision.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	source := Source{Name: "claude-project-memory", Type: "filesystem", Path: root, Scope: "project:mora"}
	if _, err := ingestFilesystemDetailed(context.Background(), cfg, source, io.Discard); err != nil {
		t.Fatal(err)
	}
	manifest := filesystemManifestPath(cfg, source.Name)
	manifestBefore, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	id := "src_" + ContentHash(source.Name+":decision.md")
	path := filepath.Join(sourcesRoot(cfg), "filesystem", source.Name, id+".md")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(native); err != nil {
		t.Fatal(err)
	}
	_, err = ingestFilesystemDetailed(context.Background(), cfg, source, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "symlink target") {
		t.Fatalf("broken target reported as a clean unchanged source: %v", err)
	}
	after, readErr := os.ReadFile(path)
	manifestAfter, manifestErr := os.ReadFile(manifest)
	if readErr != nil || manifestErr != nil || string(before) != string(after) || string(manifestBefore) != string(manifestAfter) {
		t.Fatalf("failed scan pruned retained data/manifest: %v, %v", readErr, manifestErr)
	}
}
