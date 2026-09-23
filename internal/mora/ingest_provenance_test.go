package mora

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFilesystemRenderErrorPreservesPriorRecordAndManifest(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	root := t.TempDir()
	path := filepath.Join(root, "note.md")
	if err := os.WriteFile(path, []byte("version one"), 0o644); err != nil {
		t.Fatal(err)
	}
	source := Source{Name: "docs", Type: "filesystem", Path: root, Scope: "personal"}
	if _, err := ingestFilesystemDetailed(context.Background(), cfg, source, io.Discard); err != nil {
		t.Fatal(err)
	}
	id := "src_" + ContentHash(source.Name+":note.md")
	recordPath := filepath.Join(sourcesRoot(cfg), source.Type, source.Name, id+".md")
	beforeRecord, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filesystemManifestPath(cfg, source.Name)
	beforeManifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("version two"), 0o644); err != nil {
		t.Fatal(err)
	}
	source.Scope = "personal\nid: gmail_thread/forged"
	result, err := ingestFilesystemDetailed(context.Background(), cfg, source, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "frontmatter field scope contains a line break") || result.Failed == 0 {
		t.Fatalf("render failure = %+v, %v", result, err)
	}
	afterRecord, err := os.ReadFile(recordPath)
	if err != nil || !bytes.Equal(beforeRecord, afterRecord) {
		t.Fatalf("prior record changed after render failure: %v", err)
	}
	afterManifest, err := os.ReadFile(manifestPath)
	if err != nil || !bytes.Equal(beforeManifest, afterManifest) {
		t.Fatalf("manifest changed after render failure: %v", err)
	}
}
