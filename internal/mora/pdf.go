package mora

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/pyranthus-hq/mora/internal/memory"
	"github.com/pyranthus-hq/mora/internal/pdftext"
)

// PDF extraction caps. Package vars (not consts) so tests can lower them.
// pdfMaxFileSize rejects the file before parsing (a hostile PDF can demand large
// allocations from tiny inputs, so the parse itself is also recover-wrapped).
// pdfMaxPages bounds work on pathological page counts; extraction truncates to
// the first pdfMaxPages pages. Extracted text additionally obeys the existing
// 512 KiB index bound at every call site (over → the whole file is skipped,
// exactly like the .docx and raw-read paths).
var (
	pdfMaxFileSize                     int64 = 20 << 20 // 20 MiB
	pdfMaxPages                              = 500
	testHookAttachmentAfterParentCheck func()
)

// extractPDFText adapts Mora's test-adjustable caps to the bounded leaf extractor.
func extractPDFText(path string) (string, error) {
	return pdftext.Extract(path, pdftext.Options{MaxFileSize: pdfMaxFileSize, MaxPages: pdfMaxPages, MaxTextBytes: 512 * 1024})
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
	// Fast-path an already-forgotten parent before PDF extraction. Correctness does
	// not rely on this one-shot check: every derived write below carries the parent
	// atoms and rechecks them under writeMappedMemory's governance lease, which
	// closes a forget racing after this check (#115).
	if sup, _, err := shouldSuppressWrite(cfg, parent); err != nil {
		return 0, err
	} else if sup {
		return 0, nil
	}
	if testHookAttachmentAfterParentCheck != nil {
		testHookAttachmentAfterParentCheck()
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
		parentAtoms := counterpartyAtoms(parent.Provider, parent.Meta)
		atomRows := make([]map[string]string, 0, len(parentAtoms))
		for _, atom := range parentAtoms {
			atomRows = append(atomRows, map[string]string{"kind": atom.Kind, "value": atom.Value})
		}
		governanceMeta := map[string]any{
			governanceParentStableIDKey: parent.StableID,
			governanceParentProviderKey: parent.Provider,
			governanceParentAtomsKey:    atomRows,
		}
		metaJSON, err := memory.CanonicalMeta(governanceMeta)
		if err != nil {
			return count, err
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
			ContentHash: memory.ContentHash(title, text, metaJSON),
			Meta:        governanceMeta,
		}
		if err := writeMappedMemory(cfg, mm); err != nil {
			return count, err
		}
		out := filepath.Join(sourcesRoot(cfg), mm.Provider, osSafeBase(memory.SafeFilename(mm.StableID))+".md")
		if _, err := os.Stat(out); err == nil {
			count++
		}
	}
	return count, nil
}
