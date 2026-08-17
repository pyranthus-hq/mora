package mora

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pyranthus-hq/mora/internal/memory"
)

// writeMinimalPDF hand-builds a tiny valid PDF (uncompressed content streams,
// Helvetica, correct xref offsets) with one page per entry in pageTexts. Texts
// must not contain parentheses or backslashes.
func writeMinimalPDF(t *testing.T, path string, pageTexts ...string) {
	t.Helper()
	n := len(pageTexts)
	// Objects: 1 Catalog, 2 Pages, 3 Font, 4..3+n Page, 4+n..3+2n Contents.
	objs := make([]string, 0, 3+2*n)
	kids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		kids = append(kids, fmt.Sprintf("%d 0 R", 4+i))
	}
	objs = append(objs, "<< /Type /Catalog /Pages 2 0 R >>")
	objs = append(objs, fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), n))
	objs = append(objs, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	for i := 0; i < n; i++ {
		objs = append(objs, fmt.Sprintf(
			"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents %d 0 R /Resources << /Font << /F1 3 0 R >> >> >>",
			4+n+i))
	}
	for _, text := range pageTexts {
		stream := fmt.Sprintf("BT /F1 12 Tf 72 720 Td (%s) Tj ET", text)
		objs = append(objs, fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream))
	}

	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs)+1) // 1-indexed
	for i, o := range objs {
		offsets[i+1] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for i := 1; i <= len(objs); i++ {
		fmt.Fprintf(&buf, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWriteAttachmentMemories(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	dir := t.TempDir()
	pdfPath := filepath.Join(dir, "lease.pdf")
	writeMinimalPDF(t, pdfPath, "annual rent escalation clause")

	parent := memory.MappedMemory{
		StableID: "imessage_chat_ABC", Provider: "imessage", ProviderID: "ABC",
		Scope: "personal", Tags: []string{"imessage"},
		CreatedAt: "2026-06-01T00:00:00Z",
		Attachments: []memory.Attachment{
			{Filename: "lease.pdf", MimeType: "application/pdf", Path: pdfPath},
			{Filename: "photo.heic", MimeType: "image/heic", Path: filepath.Join(dir, "photo.heic")},     // non-PDF: ignored
			{Filename: "gone.pdf", MimeType: "application/pdf", Path: filepath.Join(dir, "missing.pdf")}, // missing file: skipped
		},
	}
	n, err := writeAttachmentMemories(cfg, parent)
	if err != nil {
		t.Fatalf("writeAttachmentMemories: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 derived memory, got %d", n)
	}

	id := "att_" + memory.ContentHash(parent.StableID+":"+pdfPath)
	out := filepath.Join(sourcesRoot(cfg), "imessage", memory.SafeFilename(id)+".md")
	m, err := parseMemory(out)
	if err != nil {
		t.Fatalf("derived memory not written at %s: %v", out, err)
	}
	if m.Title != "lease.pdf" || !strings.Contains(m.Text, "annual rent escalation clause") {
		t.Fatalf("derived memory wrong shape: title=%q", m.Title)
	}
	if m.CreatedAt != parent.CreatedAt {
		t.Fatalf("created_at must inherit the parent's: %q", m.CreatedAt)
	}
	// must carry the attachment tag
	hasTag := false
	for _, tag := range m.Tags {
		if tag == "attachment" {
			hasTag = true
		}
	}
	if !hasTag {
		t.Fatalf("derived memory must carry the attachment tag: %v", m.Tags)
	}

	// Idempotent: re-running with an unchanged file rewrites nothing.
	before, _ := os.Stat(out)
	if _, err := writeAttachmentMemories(cfg, parent); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(out)
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("unchanged pdf must content-hash-skip the rewrite")
	}
}

func TestIngestFilesystemPDF(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	dir := t.TempDir()
	writeMinimalPDF(t, filepath.Join(dir, "contract.pdf"), "signed widget contract clause")
	if err := os.WriteFile(filepath.Join(dir, "junk.pdf"), []byte("not a pdf at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeMinimalPDF(t, filepath.Join(dir, "MEMO.PDF"), "uppercase extension memo body")

	s := fsSource("fsdocs", dir, "personal")
	n, err := ingestFilesystem(cfg, s, nil)
	if err != nil {
		t.Fatalf("ingestFilesystem: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected both valid pdfs ingested (junk skipped), got %d", n)
	}

	root := filepath.Join(sourcesRoot(cfg), s.Type, s.Name)
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("reading ingested memories: %v", err)
	}
	foundLower := false
	foundUpper := false
	for _, e := range entries {
		m, perr := parseMemory(filepath.Join(root, e.Name()))
		if perr != nil {
			t.Fatalf("parseMemory(%s): %v", e.Name(), perr)
		}
		if strings.Contains(m.Text, "signed widget contract clause") {
			foundLower = true
		}
		if strings.Contains(m.Text, "uppercase extension memo body") {
			foundUpper = true
		}
	}
	if !foundLower {
		t.Fatal("extracted pdf text not found in ingested memories")
	}
	if !foundUpper {
		t.Fatal("uppercase-extension pdf text not found in ingested memories")
	}
}
