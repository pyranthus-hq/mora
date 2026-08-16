package mora

import (
	"context"
	"database/sql"
	segmentspkg "github.com/pyranthus-hq/mora/internal/segments"
)

// Evidence-segment derivation and caller-transaction-bound persistence live in
// internal/segments. Mora retains rebuild/upsert transaction ownership, index
// opening, provider orchestration, read-memory shaping, and search/MCP policy.
const (
	gmailSegDiagTruncated        = segmentspkg.DiagTruncated
	gmailSegDiagCountMismatch    = segmentspkg.DiagCountMismatch
	gmailSegDiagOrderingMismatch = segmentspkg.DiagOrderingMismatch
	gmailSegDiagMalformedRef     = segmentspkg.DiagMalformedRef
	gmailSegDiagDuplicateRef     = segmentspkg.DiagDuplicateRef
)

type gmailSegmentRow = segmentspkg.Row
type gmailSegmentDiagnostic = segmentspkg.Diagnostic
type gmailSegStmts = segmentspkg.Statements

var gmailSegSchemaStmts = segmentspkg.SchemaStatements
var gmailSegDeleteStmts = segmentspkg.DeleteStatements

func deriveIMessageSegments(m Memory) ([]gmailSegmentRow, *gmailSegmentDiagnostic) {
	return segmentspkg.Derive(m)
}
func imessageDirection(refs []string) string { return segmentspkg.Direction(refs) }
func prepareGmailSegStmts(ctx context.Context, tx *sql.Tx) (*gmailSegStmts, error) {
	return segmentspkg.Prepare(ctx, tx)
}
func writeGmailSegments(ctx context.Context, stmts *gmailSegStmts, m Memory) error {
	return stmts.Write(ctx, m)
}
func clearGmailSegmentsFor(ctx context.Context, tx *sql.Tx, memoryID string) error {
	return segmentspkg.Clear(ctx, tx, memoryID)
}
func gmailSegmentByRef(ctx context.Context, cfg Config, memoryID, evidenceRef string) (gmailSegmentRow, bool, error) {
	db, err := openIndexRO(ctx, cfg)
	if err != nil {
		return gmailSegmentRow{}, false, err
	}
	defer db.Close()
	return segmentspkg.Lookup(ctx, db, memoryID, evidenceRef)
}
