package google

import (
	"encoding/json"

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
	if mapped.Meta == nil {
		mapped.Meta = make(map[string]any)
	}
	mapped.Meta["activity_stamp"] = activity.StampMeta(memory.Memory{ID: mapped.StableID, Provider: mapped.Provider, Type: mapped.Type, Text: mapped.Body, Meta: mapped.Meta, Truncated: mapped.Truncated})
	// Both stamps and newly retained header basis facts are additive metadata.
	// Keep the old semantic hash so an unchanged historical thread is skipped,
	// while its freshly mapped output still carries the richer facts if written.
	mapped.ContentHash = memory.MapItem(itemWithoutAutomationHeaders(it), scope, bodyBudget).ContentHash
	return mapped
}

func itemWithoutAutomationHeaders(it memory.Item) memory.Item {
	if it.Meta == nil {
		return it
	}
	// Gmail's normal mapper uses this typed slice. Preserve that concrete shape:
	// json.Marshal renders struct fields in declaration order, unlike a map.
	if messages, ok := it.Meta["messages"].([]gmailMessageEvidence); ok {
		meta := make(map[string]any, len(it.Meta))
		for key, value := range it.Meta {
			meta[key] = value
		}
		copyMessages := append([]gmailMessageEvidence(nil), messages...)
		for i := range copyMessages {
			copyMessages[i].AutomationHeaders = nil
		}
		meta["messages"] = copyMessages
		it.Meta = meta
		return it
	}
	// Parsed JSON/YAML metadata has map/slice values. There JSON's sorted map
	// encoding is already its legacy form, so a generic copy is byte-stable.
	encoded, err := json.Marshal(it.Meta)
	if err != nil {
		return it
	}
	var copied map[string]any
	if json.Unmarshal(encoded, &copied) != nil {
		return it
	}
	messages, ok := copied["messages"].([]any)
	if !ok {
		return it
	}
	for _, raw := range messages {
		if row, ok := raw.(map[string]any); ok {
			delete(row, "automation_headers")
		}
	}
	it.Meta = copied
	return it
}
