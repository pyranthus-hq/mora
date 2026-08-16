package mora

import (
	"context"
	"database/sql"
	segmentspkg "github.com/pyranthus-hq/mora/internal/segments"
)

const (
	gmailSegmentFusionK            = segmentspkg.FusionK
	gmailSegmentParentWeight       = segmentspkg.ParentWeight
	gmailSegmentArmWeight          = segmentspkg.ArmWeight
	gmailSegmentDefaultParentPool  = segmentspkg.DefaultParentPool
	gmailSegmentEvidenceIDChunkLen = segmentspkg.EvidenceIDChunkLen
)

func gmailSegmentQueryArm(ctx context.Context, db *sql.DB, query, scope string, filters ...searchFilters) ([]string, map[string]GmailSegmentEvidence, error) {
	return gmailSegmentQueryArmBounded(ctx, db, query, scope, gmailSegmentDefaultParentPool, filters...)
}
func gmailSegmentQueryArmBounded(ctx context.Context, db *sql.DB, query, scope string, pool int, filters ...searchFilters) ([]string, map[string]GmailSegmentEvidence, error) {
	return segmentspkg.Query(ctx, db, query, scope, pool, oneFilter(filters), searchSnippetLen)
}

func completeGmailSegmentEvidence(ctx context.Context, db *sql.DB, query, scope string, rows []Memory, evidence map[string]GmailSegmentEvidence, filters ...searchFilters) map[string]GmailSegmentEvidence {
	return segmentspkg.CompleteEvidence(ctx, db, query, scope, rows, evidence, oneFilter(filters), searchSnippetLen)
}
func admitGmailSegmentCandidates(ctx context.Context, db *sql.DB, candidates []Memory, ids []string, pool int) ([]Memory, error) {
	return segmentspkg.AdmitCandidates(ctx, db, candidates, ids, pool, parseMemory)
}
func fuseGmailSegmentArm(candidates []Memory, parentIDs, segmentIDs []string) []Memory {
	return segmentspkg.FuseCandidates(candidates, parentIDs, segmentIDs)
}
func attachGmailSegmentEvidence(rows []Memory, evidence map[string]GmailSegmentEvidence) {
	segmentspkg.AttachEvidence(rows, evidence)
}
