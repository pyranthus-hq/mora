package mora

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ledongthuc/pdf"
	"github.com/pyranthus-hq/mora/internal/memory"
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

// isPDFAttachment gates extraction on MIME or extension — chat.db rows sometimes
// carry one without the other.
func isPDFAttachment(a memory.Attachment) bool {
	if strings.EqualFold(a.MimeType, "application/pdf") {
		return true
	}
	return strings.EqualFold(filepath.Ext(a.Filename), ".pdf")
}

// writeAttachmentMemories writes one derived memory per extractable PDF attachment
// of parent (design: docs/superpowers/specs/2026-06-11-pdf-ingestion-design.md).
// Every extraction failure — missing file, malformed, encrypted, empty/scanned,
// past the caps — skips that attachment and keeps the metadata on the parent; a
// body we can't read is not a sync error. Only a vault write failure propagates.
// The stable ID hashes parent StableID + path, so re-syncs hit the content-hash
// skip in writeMappedMemory and an unchanged PDF is a no-op.
func writeAttachmentMemories(cfg Config, parent memory.MappedMemory) (int, error) {
	// Derived attachment memories carry `att_<hash>` ids that DON'T inherit the
	// parent's participant Meta, so the per-item guard in writeMappedMemory can't
	// see they belong to a forgotten chat. Consult the parent's suppression here
	// so a forgotten conversation's PDFs are not smuggled in through this 5th
	// (derived) write path (#52).
	if sup, _, err := shouldSuppressWrite(cfg, parent); err != nil {
		return 0, err
	} else if sup {
		return 0, nil
	}
	count := 0
	for _, a := range parent.Attachments {
		if a.Path == "" || !isPDFAttachment(a) {
			continue
		}
		text, err := extractPDFText(a.Path)
		if err != nil || text == "" || len(text) > 512*1024 {
			continue
		}
		title := a.Filename
		if title == "" {
			title = filepath.Base(a.Path)
		}
		mm := memory.MappedMemory{
			StableID:    "att_" + memory.ContentHash(parent.StableID+":"+a.Path),
			Type:        "source",
			Title:       title,
			Body:        text,
			Tags:        append(append([]string{}, parent.Tags...), "attachment"),
			Source:      a.Path,
			Provider:    parent.Provider,
			ProviderID:  parent.ProviderID,
			Account:     parent.Account,
			Scope:       parent.Scope,
			CreatedAt:   parent.CreatedAt,
			ContentHash: memory.ContentHash(title, text),
		}
		if err := writeMappedMemory(cfg, mm); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}
