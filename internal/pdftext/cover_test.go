package pdftext

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mcBuildPDF(streams []string, nullKid bool) []byte {
	n := len(streams)
	count := n
	kids := make([]string, 0, n+1)
	for i := 0; i < n; i++ {
		kids = append(kids, fmt.Sprintf("%d 0 R", 4+i))
	}
	if nullKid {
		kids = append(kids, "999 0 R") // dangling reference → resolves to Null
		count = n + 1
	}
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), count),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	for i := 0; i < n; i++ {
		objs = append(objs, fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents %d 0 R /Resources << /Font << /F1 3 0 R >> >> >>", 4+n+i))
	}
	for _, s := range streams {
		objs = append(objs, fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(s), s))
	}
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs)+1)
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
	return buf.Bytes()
}

func TestMc_ExtractPDFTextTruncates(t *testing.T) {
	p := filepath.Join(t.TempDir(), "huge.pdf")
	// Page 1 (~450 KiB) stays under the cap; page 2 pushes the buffer past it,
	// tripping the break; page 3 must never be read.
	page1 := strings.Repeat("onexx ", 78000) // ~468 KiB
	page2 := strings.Repeat("twoxx ", 20000) // ~120 KiB -> total > 512 KiB
	page3 := strings.Repeat("three3 ", 100)  // sentinel that must be dropped
	writeMinimalPDF(t, p, page1, page2, page3)

	got, err := extractPDFText(p)
	if err != nil {
		t.Fatalf("extractPDFText: %v", err)
	}
	if !strings.Contains(got, "onexx") {
		t.Fatal("page 1 text missing before the cap")
	}
	if !strings.Contains(got, "twoxx") {
		t.Fatal("page 2 text (which trips the cap) missing")
	}
	if strings.Contains(got, "three3") {
		t.Fatal("512 KiB cap not enforced: page 3 was read past the break")
	}
}

func TestMc_ExtractPDFTextSkipsNullPage(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nullpage.pdf")
	if err := os.WriteFile(p, mcBuildPDF([]string{"BT /F1 12 Tf (real page text) Tj ET"}, true), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := extractPDFText(p)
	if err != nil {
		t.Fatalf("a null page must be skipped, not error: %v", err)
	}
	if !strings.Contains(got, "real page text") {
		t.Fatalf("real page text lost when skipping the null page, got %q", got)
	}
}

func TestMc_ExtractPDFTextSkipsGarbledPage(t *testing.T) {
	p := filepath.Join(t.TempDir(), "garbled.pdf")
	streams := []string{
		"BT /F1 Tf (garbled) Tj ET", // Tf missing its size arg → interpreter panics
		"BT /F1 12 Tf (surviving page) Tj ET",
	}
	if err := os.WriteFile(p, mcBuildPDF(streams, false), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := extractPDFText(p)
	if err != nil {
		t.Fatalf("a garbled page must be skipped, not error: %v", err)
	}
	if strings.Contains(got, "garbled") {
		t.Fatalf("the garbled page should have been skipped, got %q", got)
	}
	if !strings.Contains(got, "surviving page") {
		t.Fatalf("the valid page after a garbled one must survive, got %q", got)
	}
}

func TestMc_ExtractPDFTextRecoversPanic(t *testing.T) {
	b := mcBuildPDF([]string{"BT /F1 12 Tf (gamma) Tj ET"}, false)
	s := string(b)
	// Corrupt object 2 (the Pages dict) xref offset so resolving /Pages panics.
	xi := strings.Index(s, "xref\n0 ")
	if xi < 0 {
		t.Fatal("xref table not found in generated PDF")
	}
	hdrEnd := strings.IndexByte(s[xi+5:], '\n') // end of the "0 N" subsection header
	entriesStart := xi + 5 + hdrEnd + 1
	obj2Off := entriesStart + 20 + 20 // each xref entry is exactly 20 bytes; skip obj 0 and 1
	corrupt := s[:obj2Off] + "0000000009" + s[obj2Off+10:]

	p := filepath.Join(t.TempDir(), "badoffset.pdf")
	if err := os.WriteFile(p, []byte(corrupt), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := extractPDFText(p)
	if err == nil {
		t.Fatal("a parser panic must surface as an error, not crash")
	}
	if !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("expected a recovered-panic error, got %v", err)
	}
}
