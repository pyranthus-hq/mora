package mora

import (
	"fmt"
	"os"
	"strings"

	"github.com/ledongthuc/pdf"
)

// PDF extraction caps. Package vars (not consts) so tests can lower them.
// pdfMaxFileSize rejects the file before parsing (a hostile PDF can demand large
// allocations from tiny inputs, so the parse itself is also recover-wrapped).
// pdfMaxPages bounds work on pathological page counts; extraction truncates to
// the first pdfMaxPages pages. Extracted text additionally obeys the existing
// 512 KiB index bound at every call site (over → the whole file is skipped,
// exactly like the .docx and raw-read paths).
var (
	pdfMaxFileSize int64 = 20 << 20 // 20 MiB
	pdfMaxPages          = 500
)

// extractPDFText returns the plain text of a PDF using the pinned, audited
// ledongthuc/pdf (see docs/superpowers/plans/2026-06-11-pdf-ingestion.md for the
// supply-chain record). The library panics on malformed input by design, so the
// whole parse is recover-wrapped: any panic becomes an error and the caller skips
// the file — a bad PDF must never crash a sync. Scanned/image-only PDFs extract
// to "" with a nil error; callers skip on empty (never index garbage). No OCR —
// that would break the no-CGO/single-binary constraint.
func extractPDFText(path string) (text string, err error) {
	defer func() {
		if r := recover(); r != nil {
			text, err = "", fmt.Errorf("pdf parse panicked (malformed input): %v", r)
		}
	}()
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if fi.Size() > pdfMaxFileSize {
		return "", fmt.Errorf("pdf too large: %d bytes (cap %d)", fi.Size(), pdfMaxFileSize)
	}
	f, r, err := pdf.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	var b strings.Builder
	n := r.NumPage()
	if n > pdfMaxPages {
		n = pdfMaxPages
	}
	for i := 1; i <= n; i++ {
		p := r.Page(i)
		if p.V.IsNull() {
			continue
		}
		fonts := make(map[string]*pdf.Font)
		for _, name := range p.Fonts() {
			font := p.Font(name)
			fonts[name] = &font
		}
		s, perr := p.GetPlainText(fonts)
		if perr != nil {
			continue // one garbled page must not lose the rest of the document
		}
		b.WriteString(s)
		b.WriteString("\n")
		if b.Len() > 512*1024 {
			break // truncate here; the caller's 512 KiB cap skips files whose extracted text exceeds the index bound.
		}
	}
	return strings.TrimSpace(b.String()), nil
}
