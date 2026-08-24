package mora

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestFilesystemIncrementalManifestSkipsUnchangedRecords(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	root := t.TempDir()
	path := filepath.Join(root, "note.md")
	if err := os.WriteFile(path, []byte("version one"), 0o644); err != nil {
		t.Fatal(err)
	}
	source := Source{Name: "docs", Type: "filesystem", Path: root, Scope: "personal"}

	first, err := ingestFilesystemDetailed(context.Background(), cfg, source, io.Discard)
	if err != nil || first.Materialized != 1 {
		t.Fatalf("first incremental ingest=%+v err=%v", first, err)
	}
	second, err := ingestFilesystemDetailed(context.Background(), cfg, source, io.Discard)
	if err != nil || second.Examined != 0 || second.Materialized != 0 {
		t.Fatalf("no-op incremental ingest=%+v err=%v", second, err)
	}

	if err := os.WriteFile(path, []byte("version two is changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	third, err := ingestFilesystemDetailed(context.Background(), cfg, source, io.Discard)
	if err != nil || third.Examined != 1 || third.Materialized != 1 {
		t.Fatalf("changed incremental ingest=%+v err=%v", third, err)
	}
	if _, err := os.Stat(filesystemManifestPath(cfg, source.Name)); err != nil {
		t.Fatalf("durable manifest: %v", err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	fourth, err := ingestFilesystemDetailed(context.Background(), cfg, source, io.Discard)
	if err != nil || fourth.Examined != 1 || fourth.Materialized != 1 {
		t.Fatalf("deleted incremental ingest=%+v err=%v", fourth, err)
	}
	id := "src_" + ContentHash(source.Name+":note.md")
	if _, err := os.Stat(filepath.Join(sourcesRoot(cfg), source.Type, source.Name, id+".md")); !os.IsNotExist(err) {
		t.Fatalf("deleted provider record remains: %v", err)
	}
}
