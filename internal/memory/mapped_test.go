package memory

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestMapItemBasics(t *testing.T) {
	occurredAt := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	it := Item{
		Kind:       kindGmailThread,
		ProviderID: "t1",
		Title:      "  Re: OAuth  ",
		Body:       "let's use oauth",
		OccurredAt: occurredAt,
		Tags:       []string{"gmail", "INBOX"},
	}

	m := MapItem(it, "personal", 1000)
	assertMappedMemoryShape(t, m)

	if m.StableID != StableID(it.Kind, it.ProviderID) {
		t.Fatalf("stable id: %s", m.StableID)
	}
	if m.Type != "email" || m.Provider != "gmail" || m.Scope != "personal" {
		t.Fatalf("type/provider/scope: %s/%s/%s", m.Type, m.Provider, m.Scope)
	}
	if m.ProviderID != it.ProviderID {
		t.Fatalf("provider id: %s", m.ProviderID)
	}
	if m.Title != "Re: OAuth" {
		t.Fatalf("title not trimmed: %q", m.Title)
	}
	if m.Body != it.Body {
		t.Fatalf("body: %q", m.Body)
	}
	if m.Truncated {
		t.Fatal("short body should not be truncated")
	}
	if m.OriginalSize != len(it.Body) {
		t.Fatalf("original size: %d", m.OriginalSize)
	}
	if m.IngestedSize != len(m.Body) {
		t.Fatalf("ingested size: %d", m.IngestedSize)
	}
	if m.ContentHash == "" {
		t.Fatal("content hash must be set")
	}
	if m.CreatedAt != occurredAt.Format(time.RFC3339) {
		t.Fatalf("created at: %q", m.CreatedAt)
	}
	if m.DeletedAt != "" {
		t.Fatalf("live item should not carry DeletedAt: %q", m.DeletedAt)
	}
}

func TestMapItemCalendarTypeProviderAndCollections(t *testing.T) {
	occurredAt := time.Date(2026, 5, 3, 15, 30, 0, 0, time.UTC)
	attachments := []Attachment{
		{Filename: "agenda.pdf", MimeType: "application/pdf", Size: 42},
	}
	meta := map[string]any{"calendar_id": "primary", "recurrence": "weekly"}
	tags := []string{"zeta", "alpha"}
	it := Item{
		Kind:        kindCalEvent,
		ProviderID:  "c1/e1",
		Title:       "  Standup  ",
		Body:        "sync on work",
		OccurredAt:  occurredAt,
		Tags:        tags,
		Attachments: attachments,
		Meta:        meta,
	}

	m := MapItem(it, "work", 1000)

	if m.StableID != StableID(it.Kind, it.ProviderID) {
		t.Fatalf("stable id: %s", m.StableID)
	}
	if m.Type != "event" || m.Provider != "calendar" || m.Scope != "work" {
		t.Fatalf("type/provider/scope: %s/%s/%s", m.Type, m.Provider, m.Scope)
	}
	if m.Title != "Standup" {
		t.Fatalf("title not trimmed: %q", m.Title)
	}
	if m.CreatedAt != occurredAt.Format(time.RFC3339) {
		t.Fatalf("created at: %q", m.CreatedAt)
	}
	if !reflect.DeepEqual(m.Tags, []string{"alpha", "zeta"}) {
		t.Fatalf("tags should be sorted copy: %#v", m.Tags)
	}
	if len(m.Tags) == 0 || &m.Tags[0] == &tags[0] {
		t.Fatal("tags should not alias input slice")
	}
	if !reflect.DeepEqual(m.Attachments, attachments) {
		t.Fatalf("attachments: %#v", m.Attachments)
	}
	if len(m.Attachments) == 0 || &m.Attachments[0] == &attachments[0] {
		t.Fatal("attachments should not alias input slice")
	}
	if !reflect.DeepEqual(m.Meta, meta) {
		t.Fatalf("meta: %#v", m.Meta)
	}
	meta["calendar_id"] = "changed"
	if m.Meta["calendar_id"] != "primary" {
		t.Fatal("meta should not alias input map")
	}
}

// TestMapItemUnregisteredKindDerivesSaneDefault proves a non-google kind (e.g. a
// future iMessage connector) maps to a sane type/provider without editing this
// package — the connector-extensible kindToType contract.
func TestMapItemUnregisteredKindDerivesSaneDefault(t *testing.T) {
	it := Item{Kind: ItemKind("imessage_chat"), ProviderID: "guid-1", Title: "Chat", Body: "hi"}
	m := MapItem(it, "personal", 1000)
	if m.Provider != "imessage_chat" {
		t.Fatalf("unregistered kind should derive provider from kind, got %q", m.Provider)
	}
	if m.Type == "" {
		t.Fatal("unregistered kind should still produce a non-empty type")
	}
	if m.StableID != "imessage_chat/guid-1" {
		t.Fatalf("stable id should use the kind string: %q", m.StableID)
	}
}

// TestRegisterKindOverridesDefault proves a connector can register its kind's
// type/provider so MapItem emits them without a hard-coded switch.
func TestRegisterKindOverridesDefault(t *testing.T) {
	RegisterKind(ItemKind("test_registered"), "conversation", "testprov")
	t.Cleanup(func() { delete(kindRegistry, ItemKind("test_registered")) })
	m := MapItem(Item{Kind: ItemKind("test_registered"), ProviderID: "x"}, "personal", 0)
	if m.Type != "conversation" || m.Provider != "testprov" {
		t.Fatalf("registered kind should map to (conversation, testprov), got (%s, %s)", m.Type, m.Provider)
	}
}

func TestMapItemTruncates(t *testing.T) {
	body := strings.Repeat("x", 5000)
	m := MapItem(Item{Kind: kindGmailThread, ProviderID: "t2", Title: "big", Body: body}, "personal", 1000)

	if !m.Truncated {
		t.Fatal("expected truncation")
	}
	if len(m.Body) > 1000 {
		t.Fatalf("body not capped: %d", len(m.Body))
	}
	if m.OriginalSize != 5000 {
		t.Fatalf("original size: %d", m.OriginalSize)
	}
	if m.IngestedSize != len(m.Body) {
		t.Fatalf("ingested size: %d", m.IngestedSize)
	}
}

func TestMapItemDoesNotTruncateWithoutPositiveBudget(t *testing.T) {
	body := strings.Repeat("x", 5000)
	m := MapItem(Item{Kind: kindGmailThread, ProviderID: "t3", Title: "unlimited", Body: body}, "personal", 0)

	if m.Truncated {
		t.Fatal("zero body budget should not truncate")
	}
	if m.Body != body {
		t.Fatalf("body should be unchanged, got length %d", len(m.Body))
	}
	if m.OriginalSize != len(body) {
		t.Fatalf("original size: %d", m.OriginalSize)
	}
	if m.IngestedSize != len(body) {
		t.Fatalf("ingested size: %d", m.IngestedSize)
	}
}

func TestMapItemTombstone(t *testing.T) {
	m := MapItem(Item{Kind: kindCalEvent, ProviderID: "c1/e1", Title: "Standup", Deleted: true}, "personal", 1000)

	if m.DeletedAt == "" {
		t.Fatal("deleted item must carry DeletedAt")
	}
	if _, err := time.Parse(time.RFC3339, m.DeletedAt); err != nil {
		t.Fatalf("DeletedAt must be RFC3339: %q: %v", m.DeletedAt, err)
	}
}

func assertMappedMemoryShape(t *testing.T, m MappedMemory) {
	t.Helper()
	_ = []string{
		m.StableID,
		m.Scope,
		m.Type,
		m.Title,
		m.Body,
		m.Source,
		m.CreatedAt,
		m.Provider,
		m.ProviderID,
		m.ContentHash,
		m.LastSynced,
		m.DeletedAt,
	}
	_ = []int{m.OriginalSize, m.IngestedSize}
	_ = []bool{m.Truncated}
	_ = []Attachment{}
	_ = m.Tags
	_ = m.Attachments
	_ = m.Meta
}
