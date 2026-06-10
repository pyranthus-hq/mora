package google

import "github.com/pyranthus-hq/mora/internal/memory"

// SyncStatus and its load/save helpers now live in internal/memory (shared
// across connectors). Re-exported so existing google call-sites read unchanged.
type SyncStatus = memory.SyncStatus

var (
	LoadStatus = memory.LoadStatus
	SaveStatus = memory.SaveStatus
)
