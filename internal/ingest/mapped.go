package ingest

import (
	"path/filepath"

	"github.com/pyranthus-hq/mora/internal/config"
	"github.com/pyranthus-hq/mora/internal/memory"
	"github.com/pyranthus-hq/mora/internal/memoryfile"
)

// MappedTargetPath returns the canonical connector-memory publication path.
func MappedTargetPath(cfg config.Config, mm memory.MappedMemory) string {
	return filepath.Join(memoryfile.SourcesRoot(cfg), mm.Provider, memoryfile.OSSafeBase(memory.SafeFilename(mm.StableID))+".md")
}

// PrepareMapped converts a connector record and decides whether publication can be skipped.
func PrepareMapped(mm memory.MappedMemory, existing *memory.Memory) (memory.Memory, bool) {
	m := memory.Memory{ID: mm.StableID, Scope: mm.Scope, Type: mm.Type, Title: mm.Title, Tags: mm.Tags, Source: mm.Source, CreatedAt: mm.CreatedAt, Text: mm.Body, Provider: mm.Provider, Account: mm.Account, ProviderID: mm.ProviderID, ContentHash: mm.ContentHash, LastSynced: mm.LastSynced, Truncated: mm.Truncated, DeletedAt: mm.DeletedAt, Meta: mm.Meta}
	if existing == nil {
		return m, false
	}
	evidenceMigration := (mm.Provider == "imessage" || mm.Provider == "whatsapp") && existing.Meta["message_evidence_schema"] == nil && mm.Meta["message_evidence_schema"] != nil
	if existing.ContentHash == mm.ContentHash && mm.DeletedAt == "" && !evidenceMigration {
		return m, true
	}
	m.CreatedAt = existing.CreatedAt
	return m, false
}
