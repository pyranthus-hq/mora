package google

import "github.com/pyranthus-hq/mora/internal/memory"

// MappedMemory and MapItem now live in internal/memory (shared across
// connectors). Re-exported here so existing google call-sites read unchanged.
type MappedMemory = memory.MappedMemory

// MapItem converts a fetched Item into a MappedMemory, applying a byte budget.
var MapItem = memory.MapItem
