package mora

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestExtractPDFText(t *testing.T) {
	p := filepath.Join(t.TempDir(), "doc.pdf")
	writeMinimalPDF(t, p, "alpha lease agreement", "bravo renewal terms")
	got, err := extractPDFText(p)
	if err != nil {
		t.Fatalf("extractPDFText: %v", err)
	}
	if !strings.Contains(got, "alpha lease agreement") || !strings.Contains(got, "bravo renewal terms") {
		t.Fatalf("expected both pages' text, got %q", got)
	}
}

func TestExtractPDFTextMalformedIsErrorNotPanic(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.pdf")
	if err := os.WriteFile(p, []byte("%PDF-1.4 this is not a real pdf"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := extractPDFText(p); err == nil {
		t.Fatal("expected an error for a malformed pdf, got nil")
	}
	// Reaching here at all proves no panic escaped.
}

func TestExtractPDFTextOversizedIsError(t *testing.T) {
	p := filepath.Join(t.TempDir(), "big.pdf")
	writeMinimalPDF(t, p, "small but capped")
	old := pdfMaxFileSize
	pdfMaxFileSize = 8 // bytes — anything real exceeds this
	defer func() { pdfMaxFileSize = old }()
	if _, err := extractPDFText(p); err == nil {
		t.Fatal("expected an error past the size cap, got nil")
	}
}

func TestExtractPDFTextPageCap(t *testing.T) {
	p := filepath.Join(t.TempDir(), "many.pdf")
	writeMinimalPDF(t, p, "page one text", "page two text", "page three text")
	old := pdfMaxPages
	pdfMaxPages = 2
	defer func() { pdfMaxPages = old }()
	got, err := extractPDFText(p)
	if err != nil {
		t.Fatalf("extractPDFText: %v", err)
	}
	if !strings.Contains(got, "page one text") || !strings.Contains(got, "page two text") {
		t.Fatalf("expected first two pages, got %q", got)
	}
	if strings.Contains(got, "page three text") {
		t.Fatalf("page cap not enforced: %q", got)
	}
}

func TestExtractPDFTextEmptyIsNotError(t *testing.T) {
	p := filepath.Join(t.TempDir(), "empty.pdf")
	writeMinimalPDF(t, p) // zero pages — extracts to nothing, like a scanned PDF
	got, err := extractPDFText(p)
	if err != nil {
		t.Fatalf("empty pdf must not error (callers skip on empty): %v", err)
	}
	if strings.TrimSpace(got) != "" {
		t.Fatalf("expected empty text, got %q", got)
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
	n, err := ingestFilesystem(cfg, s)
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
