package mora

import (
	"archive/zip"
	"github.com/pyranthus-hq/mora/internal/genericutil"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeDocx builds a minimal valid .docx (a zip whose word/document.xml carries the
// given paragraphs as <w:t> runs) at path — enough for the extractor under test.
func writeDocx(t *testing.T, path string, paragraphs ...string) {
	t.Helper()
	var body strings.Builder
	body.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	body.WriteString(`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>`)
	for _, p := range paragraphs {
		body.WriteString(`<w:p><w:r><w:t xml:space="preserve">`)
		body.WriteString(p)
		body.WriteString(`</w:t></w:r></w:p>`)
	}
	body.WriteString(`</w:body></w:document>`)
	writeDocxRaw(t, path, body.String())
}

// writeDocxRaw writes a .docx zip with an arbitrary document.xml payload (used to
// exercise malformed and oversized inputs).
func writeDocxRaw(t *testing.T, path, documentXML string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(documentXML)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

func fsSource(name, path, scope string) Source {
	return Source{Name: name, Type: "filesystem", Scope: scope, Path: path, Enabled: genericutil.Ptr(true), CreatedAt: time.Now().Format(time.RFC3339)}
}

// --- Feature 1: .docx text extraction ------------------------------------------

func TestExtractDocxText(t *testing.T) {
	p := filepath.Join(t.TempDir(), "notes.docx")
	writeDocx(t, p, "First paragraph alpha", "Second paragraph bravo")
	got, err := extractDocxText(p)
	if err != nil {
		t.Fatalf("extractDocxText: %v", err)
	}
	if !strings.Contains(got, "First paragraph alpha") || !strings.Contains(got, "Second paragraph bravo") {
		t.Fatalf("expected both paragraphs in extracted text, got %q", got)
	}
	// Paragraph boundaries must separate runs so adjacent words don't fuse.
	if strings.Contains(got, "alphaSecond") {
		t.Fatalf("paragraphs were not separated: %q", got)
	}
}

func TestExtractDocxTextMalformedIsError(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.docx")
	if err := os.WriteFile(p, []byte("this is not a zip archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := extractDocxText(p); err == nil {
		t.Fatal("expected an error for a non-zip .docx, got nil")
	}
}

func TestExtractDocxTextZipBombIsGuarded(t *testing.T) {
	p := filepath.Join(t.TempDir(), "huge.docx")
	// document.xml whose text payload far exceeds the decompression cap. A correct
	// extractor stops at the cap (returns an error on the truncated XML) rather than
	// materializing the whole thing; a naive one returns the full 16 MiB.
	huge := strings.Repeat("A", 16<<20)
	xml := `<?xml version="1.0"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>` + huge + `</w:t></w:r></w:p></w:body></w:document>`
	writeDocxRaw(t, p, xml)
	got, err := extractDocxText(p)
	if err == nil && len(got) > 1<<20 {
		t.Fatalf("zip-bomb guard absent: returned %d bytes with no error", len(got))
	}
}

func TestIngestFilesystemIncludesDocx(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := testCtx(t)

	dir := t.TempDir()
	writeDocx(t, filepath.Join(dir, "report.docx"), "Pangolin quarterly synthesis numbers")

	src := fsSource("docs", dir, "personal")
	if err := saveSources(cfg, []Source{src}); err != nil {
		t.Fatal(err)
	}
	if _, err := ingestSource(cfg, src, nil); err != nil {
		t.Fatalf("ingestSource: %v", err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	out := run(t, "search", "Pangolin quarterly synthesis", "--json")
	if !strings.Contains(out, "Pangolin quarterly synthesis") {
		t.Fatalf("expected the .docx text to be ingested + searchable, got:\n%s", out)
	}
}

// --- Feature 2: filesystem freshness (no false "unavailable (sync error)") ------

func TestSyncStatusPathForFilesystem(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	p := syncStatusPathFor(cfg, fsSource("docs", "/tmp/x", "personal"))
	if p == "" {
		t.Fatal("syncStatusPathFor must return a path for filesystem sources (got empty) — the brief needs it")
	}
}

func TestIngestFilesystemWritesSyncStatus(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("# Note\nhello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := fsSource("docs", dir, "personal")
	if err := saveSources(cfg, []Source{src}); err != nil {
		t.Fatal(err)
	}
	if _, err := ingestSource(cfg, src, nil); err != nil {
		t.Fatal(err)
	}
	st := loadConnectorSyncStatus(cfg, "filesystem")
	if st == nil {
		t.Fatal("filesystem ingest must write a sync status (got nil) — the digest needs it to avoid 'unavailable'")
	}
	if st.LastSuccessAt == "" || st.LastError != "" || st.ErrorCount != 0 {
		t.Fatalf("expected a clean success status, got %+v", st)
	}
}

func TestFilesystemDigestSectionNotUnavailableAfterIngest(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("# Note\nhello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := fsSource("docs", dir, "personal")
	if err := saveSources(cfg, []Source{src}); err != nil {
		t.Fatal(err)
	}
	if _, err := ingestSource(cfg, src, nil); err != nil {
		t.Fatal(err)
	}
	d, err := buildDigest(cfg, time.Now(), briefOpts{})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, s := range d.Sections {
		if s.Source == "filesystem" {
			found = true
			if s.State == stateUnavailable {
				t.Fatalf("filesystem section must NOT be 'unavailable (sync error)' after a clean ingest; got state %q", s.State)
			}
		}
	}
	if !found {
		t.Fatal("expected a filesystem section in the digest")
	}
}
