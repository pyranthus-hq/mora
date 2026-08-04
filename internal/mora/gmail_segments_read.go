package mora

import (
	"context"
	"fmt"
	"time"
)

// Issue #243 — read_memory's evidence_ref extension (frozen interface #4,
// DQ6 §2). evidence_ref narrows the READ TARGET from the full thread body to
// ONE segment's text; #242's existing applyBoundedRead/boundedReadReceipt
// pipeline (read_bounded.go) then runs UNCHANGED over that narrowed text —
// never a bespoke new receipt struct. A ref that does not resolve to a
// derived segment of the GIVEN memory id (wrong memory, unknown ref, or a
// memory whose own segments failed closed) is an explicit, fail-closed
// error — never a silent fallback to the full body.

// lookupReadMemoryEvidenceRef resolves read_memory(id, evidence_ref=...) during
// the retrieval phase. Keeping the DB lookup separate from response shaping
// lets #245 report retrieval and assembly latency truthfully.
func lookupReadMemoryEvidenceRef(ctx context.Context, cfg Config, m Memory, evidenceRef string) (gmailSegmentRow, error) {
	seg, ok, err := gmailSegmentByRef(ctx, cfg, m.ID, evidenceRef)
	if err != nil {
		return gmailSegmentRow{}, err
	}
	if !ok {
		return gmailSegmentRow{}, fmt.Errorf("evidence_ref %q is not a derived segment of memory %q (unknown ref, a different memory's ref, or this memory's segments failed closed at index time)", evidenceRef, m.ID)
	}
	return seg, nil
}

// shapeReadMemoryEvidenceRef narrows and shapes an already retrieved segment
// during the assembly phase. Plain id-only and id+bounded reads never call it.
func shapeReadMemoryEvidenceRef(cfg Config, m Memory, seg gmailSegmentRow, args map[string]any) map[string]any {
	// Narrow the target FIRST (DQ6 precedence): #242's own bounded-read
	// machinery then runs over ONLY this segment's text, so match/
	// max_tokens/occurrence — if supplied — apply strictly within it.
	scoped := m
	scoped.Text = seg.Text
	shaped, receipt := applyBoundedRead(scoped, args)
	// Identity fields survive composition with #242's params (DQ6): the
	// receipt ALWAYS carries the parent id (already receipt.ID via
	// applyBoundedRead), the requested evidence_ref, and the segment's own
	// sender/at — regardless of whether match/max_tokens/occurrence matched.
	receipt.EvidenceRef = seg.EvidenceRef
	receipt.Sender = seg.Sender
	receipt.At = seg.At
	receipt.Direction = imessageDirection(seg.BlockRefs)
	return map[string]any{"memory": shaped, "health": compactHealthOf(cfg, time.Now()), "receipt": receipt}
}
