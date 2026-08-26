package mora

import (
	"testing"
)

// Issue #218: `list_memory` is documented as "browse recent memories", but it
// ranked by `created_at` — the provider OCCURRENCE time on every connector
// memory — so future calendar fixtures led the list and agents read the single
// conflated timestamp as "when did I learn this". These tests pin the fix: the
// browse order is memory-write recency, and the row exposes the three instants
// separately, omitting any it cannot derive honestly.

// seedRecencyVault writes the fixture memories into a fresh temp-HOME vault.
// Every timestamp is fixed, so the assertions below never depend on wall clock.
func seedRecencyVault(t *testing.T, mems ...Memory) Config {
	t.Helper()
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	for _, m := range mems {
		if err := writeMemory(cfg, m); err != nil {
			t.Fatalf("seed %s: %v", m.ID, err)
		}
	}
	return cfg
}

// recencyFutureEvent reproduces the reported symptom: a calendar event dated months
// ahead, ingested days ago. Its created_at is the event START, not an ingest
// time.
func recencyFutureEvent() Memory {
	return Memory{
		ID: "cal_event_fixture", Scope: "global", Type: "event", Title: "Chelsea fixture",
		Source: "calendar", CreatedAt: "2027-01-14T15:00:00Z", Provider: "calendar",
		ProviderID: "primary/abc", ContentHash: "h-cal", LastSynced: "2026-07-29T10:00:00Z",
		Text: "When: 2027-01-14T15:00:00Z", Meta: map[string]any{"occurred_at": "2027-01-14T15:00:00Z"},
	}
}

// recencyGmailThread carries the per-message evidence a full Threads.Get persists, so
// the thread's opening message is recoverable as its source creation instant.
func recencyGmailThread() Memory {
	return Memory{
		ID: "gmail_thread_fixture", Scope: "global", Type: "email", Title: "Re: contract",
		Source: "gmail", CreatedAt: "2026-06-01T09:00:00Z", Provider: "gmail",
		ProviderID: "t1", ContentHash: "h-mail", LastSynced: "2026-07-31T08:00:00Z",
		Text: "From: neil@example.com\n\nbody",
		Meta: map[string]any{
			"occurred_at": "2026-06-01T09:00:00Z",
			"messages": []map[string]string{
				{"message_ref": "t1#0", "at": "2026-05-20T09:00:00Z"},
				{"message_ref": "t1#1", "at": "2026-06-01T09:00:00Z"},
			},
		},
	}
}

// recencyLocalNote is an agent/CLI-authored memory: no provider, so its created_at IS
// the instant of the vault write.
func recencyLocalNote() Memory {
	return Memory{
		ID: "mem_local_note", Scope: "global", Type: "insight", Title: "Local note",
		Source: "mcp", CreatedAt: "2026-07-30T12:00:00Z", Text: "a durable note",
	}
}

// recencyPDFAttachment mirrors writeAttachmentMemories: a provider memory that inherits
// its PARENT's occurrence-time created_at and carries no last_synced, so it has
// no honest write clock at all.
func recencyPDFAttachment() Memory {
	return Memory{
		ID: "att_deck_fixture", Scope: "global", Type: "source", Title: "deck.pdf",
		Source: "/tmp/deck.pdf", CreatedAt: "2027-01-14T15:00:00Z", Provider: "gmail",
		ProviderID: "t1", ContentHash: "h-att", Text: "slide text",
	}
}

// recencyIMessageChat persists only its newest message time — no thread-creation
// instant exists on disk for it.

// recencyCorruptSyncEvent is a provider memory whose write clock does not parse —
// a hand-edited or half-written frontmatter stamp. Its created_at sits right
// beside it and parses fine, but on a connector memory that value is the event
// START, so this memory must sort unknown AND publish no indexed_at rather than
// quietly borrow it.
func recencyCorruptSyncEvent() Memory {
	return Memory{
		ID: "cal_corrupt_fixture", Scope: "global", Type: "event", Title: "Corrupt sync stamp",
		Source: "calendar", CreatedAt: "2027-03-02T15:00:00Z", Provider: "calendar",
		ProviderID: "primary/corrupt", ContentHash: "h-corrupt", LastSynced: "2026-07-30 10:00:00",
		Text: "When: 2027-03-02T15:00:00Z", Meta: map[string]any{"occurred_at": "2027-03-02T15:00:00Z"},
	}
}

// recencyGoogleEventWithCreation is a Google Calendar event ingested after
// calEventToItem started persisting Event.Created: its creation instant and its
// start are months apart, and the row must show both, separately.

// TestListMemoryOrdersByWriteRecencyNotEventTime is the #218 regression: the
// future-dated calendar event must NOT lead the browse list, and the ordering
// must follow each memory's write/ingest clock. The memory with no honest write
// clock sorts last — unknown recency may not claim to be recent.
func TestListMemoryOrdersByWriteRecencyNotEventTime(t *testing.T) {
	cfg := seedRecencyVault(t, recencyFutureEvent(), recencyGmailThread(), recencyLocalNote(), recencyPDFAttachment(), recencyCorruptSyncEvent())

	// last_synced 07-31 > local created_at 07-30 > last_synced 07-29 > the two with
	// no usable write clock, which tie-break on id. The corrupt stamp reads as the
	// most recent of all if compared as raw text, so its position here is the
	// assertion that it is parsed and rejected, not string-compared.
	want := []string{"gmail_thread_fixture", "mem_local_note", "cal_event_fixture", "att_deck_fixture", "cal_corrupt_fixture"}

	got, err := listMemories(cfg, "", 0)
	if err != nil {
		t.Fatalf("listMemories: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("listMemories returned %d memories, want %d: %+v", len(got), len(want), got)
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Fatalf("browse position %d = %s, want %s (full order: %s)", i, got[i].ID, id, recencyIDs(got))
		}
	}
	// The clamp keeps the newest-WRITTEN prefix, so a limited browse can never be
	// filled by a future event.
	limited, err := listMemories(cfg, "", 2)
	if err != nil {
		t.Fatalf("listMemories(limit=2): %v", err)
	}
	if len(limited) != 2 || limited[0].ID != want[0] || limited[1].ID != want[1] {
		t.Fatalf("limit=2 kept %s, want the two newest-written %v", recencyIDs(limited), want[:2])
	}
	// Deterministic: the same vault yields the same order on a second walk.
	again, err := listMemories(cfg, "", 0)
	if err != nil {
		t.Fatalf("listMemories (repeat): %v", err)
	}
	if recencyIDs(again) != recencyIDs(got) {
		t.Fatalf("browse order is nondeterministic: %s then %s", recencyIDs(got), recencyIDs(again))
	}
}

// TestIndexedAtIsNeverFabricated pins the honesty contract on the per-memory
// clock: it is the ingest write stamp, or the local write stamp, or nothing —
// an occurrence time is never relabelled as an indexing time.

// TestBrowseRowsSplitConflatedTimestamps pins the field split: each row exposes
// the instants it can derive and omits the ones it cannot, while created_at
// stays exactly as persisted.

// TestDerivedTimestampsOmitMalformedStamps pins the output half of the
// right-or-absent contract across EVERY derivation path. Frontmatter is plain
// text on disk, so a hand edit, a truncated write, or an older binary can leave a
// stamp that does not parse; a browse row must then omit that field rather than
// hand a consumer a string it cannot parse — and must not paper over the gap with
// a neighbouring clock that answers a different question.

// TestSabotagedStampsAreOmittedFromEveryDerivedField sweeps the whole malformed
// corpus across EVERY place a derived browse field can come from (#218). The
// earlier tests pin one representative corruption per path; this one pins that
// the strict seam is the only thing any of those paths consults, so no shape —
// one-digit hour, `+00:60`, `+24:00`, comma fraction, an empty fraction, a
// mangled zone — can slip through on one field because a different field's case
// happened to be the one exercised.
//
// Each case asserts three things: the attacked field is OMITTED, no OTHER field
// shifts to cover for it, and nothing that did survive is itself malformed.

// TestListMemoryMCPRowsCarrySplitTimestamps drives the real dispatcher: the MCP
// browse rows must ship the split timestamps, in write-recency order, while the
// non-browse surfaces stay byte-identical (the derived fields are omitempty and
// only list_memory populates them).
func TestListMemoryMCPRowsCarrySplitTimestamps(t *testing.T) {
	seedRecencyVault(t, recencyFutureEvent(), recencyGmailThread(), recencyLocalNote(), recencyPDFAttachment(), recencyCorruptSyncEvent())
	ctx := testCtx(t)

	res, err := callMCPTool(ctx, "list_memory", map[string]any{})
	if err != nil {
		t.Fatalf("list_memory: %v", err)
	}
	obj, ok := res.(map[string]any)
	if !ok {
		t.Fatalf("list_memory returned %T, want the {memories, health} envelope", res)
	}
	rows, ok := obj["memories"].([]Memory)
	if !ok {
		t.Fatalf("memories = %T, want []Memory", obj["memories"])
	}
	if len(rows) != 5 {
		t.Fatalf("list_memory returned %d rows, want 5: %s", len(rows), recencyIDs(rows))
	}
	if rows[0].ID != "gmail_thread_fixture" {
		t.Fatalf("list_memory leads with %s, want the newest-written gmail_thread_fixture (order: %s)", rows[0].ID, recencyIDs(rows))
	}
	byID := map[string]Memory{}
	for _, m := range rows {
		byID[m.ID] = m
	}
	if got := byID["cal_event_fixture"]; got.EventStart != "2027-01-14T15:00:00Z" || got.IndexedAt != "2026-07-29T10:00:00Z" {
		t.Fatalf("calendar row lost its split timestamps: event_start=%q indexed_at=%q", got.EventStart, got.IndexedAt)
	}
	if got := byID["gmail_thread_fixture"]; got.SourceCreatedAt != "2026-05-20T09:00:00Z" {
		t.Fatalf("gmail row source_created_at = %q, want the opening message time", got.SourceCreatedAt)
	}
	if got := byID["att_deck_fixture"]; got.IndexedAt != "" || got.EventStart != "" || got.SourceCreatedAt != "" {
		t.Fatalf("attachment row fabricated a timestamp: %+v", got)
	}
	// A corrupt write clock is dropped end-to-end — the row still ships the fields it
	// CAN evidence, and never republishes the unparseable stamp or swaps in created_at.
	if got := byID["cal_corrupt_fixture"]; got.IndexedAt != "" || got.EventStart != "2027-03-02T15:00:00Z" {
		t.Fatalf("corrupt-last_synced row: indexed_at=%q event_start=%q, want an omitted clock and an intact start", got.IndexedAt, got.EventStart)
	}
	if rows[len(rows)-1].ID != "cal_corrupt_fixture" {
		t.Fatalf("browse order ends with %s, want the unparseable write clock sorted last (order: %s)", rows[len(rows)-1].ID, recencyIDs(rows))
	}
	// The derived fields are a browse-row concern only: read_memory returns the
	// stored record untouched.
	readRes, err := callMCPTool(ctx, "read_memory", map[string]any{"id": "cal_event_fixture"})
	if err != nil {
		t.Fatalf("read_memory: %v", err)
	}
	stored := readRes.(map[string]any)["memory"].(Memory)
	if stored.EventStart != "" || stored.SourceCreatedAt != "" || stored.IndexedAt != "" {
		t.Fatalf("read_memory leaked derived browse fields: %+v", stored)
	}
	if stored.CreatedAt != "2027-01-14T15:00:00Z" {
		t.Fatalf("read_memory created_at = %q, want the persisted value unchanged", stored.CreatedAt)
	}
}

// TestBrowseRecencyComparesInstantsNotStrings guards the zone trap: last_synced
// is always UTC while a locally minted created_at carries the machine's offset,
// so a lexical compare of "…Z" against "…-04:00" ranks them wrongly.

// recencyIDs renders an ordered id list for assertion messages.
func recencyIDs(mems []Memory) string {
	out := ""
	for i, m := range mems {
		if i > 0 {
			out += ","
		}
		out += m.ID
	}
	return out
}
