package google

import (
	"github.com/pyranthus-hq/mora/internal/activity"
	"github.com/pyranthus-hq/mora/internal/memory"
)

// MappedMemory re-exports the connector boundary DTO.
type MappedMemory = memory.MappedMemory

// MapItem adds a connector-generated activity stamp after generic truncation,
// so only retained and validated evidence is recorded. It never fetches or
// changes a provider object.
func MapItem(it memory.Item, scope string, bodyBudget int) memory.MappedMemory {
	mapped := memory.MapItem(it, scope, bodyBudget)
	mapped.Meta["activity_stamp"] = activity.StampMeta(memory.Memory{ID: mapped.StableID, Provider: mapped.Provider, Type: mapped.Type, Text: mapped.Body, Meta: mapped.Meta, Truncated: mapped.Truncated})
	// Keep the existing content hash. This intentionally preserves hash-skip for
	// historical files; a stamp is not a reason to rewrite an old vault.
	return mapped
}
