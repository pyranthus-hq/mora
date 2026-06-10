package google

import "github.com/pyranthus-hq/mora/internal/memory"

// StableID, ContentHash, and SafeFilename now live in internal/memory (shared
// across connectors). Re-exported so existing google call-sites read unchanged.
var (
	StableID     = memory.StableID
	ContentHash  = memory.ContentHash
	SafeFilename = memory.SafeFilename
)
