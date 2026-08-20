package ingest

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pyranthus-hq/mora/internal/config"
	"github.com/pyranthus-hq/mora/internal/memory"
	"github.com/pyranthus-hq/mora/internal/memoryfile"
)

func mappedFixture() memory.MappedMemory {
	return memory.MappedMemory{StableID: "gmail/thread:one?", Scope: "work", Type: "email", Title: "Subject", Body: "Body", Tags: []string{"a"}, Source: "thread", CreatedAt: "new-created", Provider: "gmail", Account: "work", ProviderID: "one", ContentHash: "hash", LastSynced: "sync", Truncated: true, DeletedAt: "", Meta: map[string]any{"x": "y"}}
}
func TestMappedTargetPath(t *testing.T) {
	cfg := config.Config{VaultDir: t.TempDir()}
	mm := mappedFixture()
	got := MappedTargetPath(cfg, mm)
	want := filepath.Join(memoryfile.SourcesRoot(cfg), "gmail", memoryfile.OSSafeBase(memory.SafeFilename(mm.StableID))+".md")
	if got != want {
		t.Fatalf("got=%q want=%q", got, want)
	}
	if strings.Contains(filepath.Base(got), "/") {
		t.Fatalf("unsafe=%q", got)
	}
}
func TestPrepareMappedCopiesCanonicalFields(t *testing.T) {
	mm := mappedFixture()
	got, skip := PrepareMapped(mm, nil)
	if skip {
		t.Fatal("new record skipped")
	}
	want := memory.Memory{ID: mm.StableID, Scope: mm.Scope, Type: mm.Type, Title: mm.Title, Tags: mm.Tags, Source: mm.Source, CreatedAt: mm.CreatedAt, Text: mm.Body, Provider: mm.Provider, Account: mm.Account, ProviderID: mm.ProviderID, ContentHash: mm.ContentHash, LastSynced: mm.LastSynced, Truncated: mm.Truncated, DeletedAt: mm.DeletedAt, Meta: mm.Meta}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got=%+v want=%+v", got, want)
	}
}
func TestPrepareMappedSkipAndRewriteDecisions(t *testing.T) {
	mm := mappedFixture()
	existing := memory.Memory{ContentHash: "hash", CreatedAt: "original", Meta: map[string]any{}}
	got, skip := PrepareMapped(mm, &existing)
	if !skip {
		t.Fatalf("unchanged rewrites: %+v", got)
	}
	mm.DeletedAt = "deleted"
	got, skip = PrepareMapped(mm, &existing)
	if skip || got.CreatedAt != "original" {
		t.Fatalf("delete=(%+v,%v)", got, skip)
	}
	mm.DeletedAt = ""
	mm.ContentHash = "changed"
	got, skip = PrepareMapped(mm, &existing)
	if skip || got.CreatedAt != "original" {
		t.Fatalf("changed=(%+v,%v)", got, skip)
	}
}
func TestPrepareMappedForcesIMessageEvidenceMigration(t *testing.T) {
	mm := mappedFixture()
	mm.Provider = "imessage"
	mm.Meta = map[string]any{"message_evidence_schema": 1}
	existing := memory.Memory{ContentHash: mm.ContentHash, CreatedAt: "original", Meta: map[string]any{}}
	got, skip := PrepareMapped(mm, &existing)
	if skip || got.CreatedAt != "original" {
		t.Fatalf("migration=(%+v,%v)", got, skip)
	}
	existing.Meta["message_evidence_schema"] = 1
	if _, skip = PrepareMapped(mm, &existing); !skip {
		t.Fatal("already migrated rewrites")
	}
}
