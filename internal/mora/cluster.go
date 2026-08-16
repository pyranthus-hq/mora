package mora

import searchpkg "github.com/pyranthus-hq/mora/internal/search"

func clusterAndTruncate(rawIDs []string, visible []Memory, limit int) []Memory {
	return searchpkg.ClusterAndTruncate(rawIDs, visible, limit, searchpkg.AnnotateLaterRelatedEvidence)
}
