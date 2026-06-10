package google

import "github.com/pyranthus-hq/mora/internal/memory"

// The resumable Ingest loop now lives in internal/memory (shared across
// connectors). Re-exported so existing google call-sites read unchanged.
type (
	IngestParams = memory.IngestParams
	IngestResult = memory.IngestResult
)

var Ingest = memory.Ingest
