package mora

import (
	"bytes"
	"context"
	"github.com/pyranthus-hq/mora/internal/atomicio"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
)

func TestIMessageEvidenceMigrationRewritesOnceWithoutBriefDelta(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	const (
		id        = "imessage_chat/chat-guid"
		createdAt = "2026-07-01T08:00:00Z"
		hash      = "stable-conversation-hash"
		body      = "## 2026-08-01\nMe: same"
	)
	dest := filepath.Join(sourcesRoot(cfg), "imessage", osSafeBase(memory.SafeFilename(id))+".md")
	legacy := Memory{
		ID: id, Scope: "personal", Type: "imessage", Title: "Alex", Source: "chat-guid",
		Provider: "imessage", ProviderID: "chat-guid", CreatedAt: createdAt,
		ContentHash: hash, Text: body, Meta: map[string]any{"message_count": "1"},
	}
	legacyBytes, err := renderMemory(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicio.Write(dest, legacyBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	mm := memory.MappedMemory{
		StableID: id, Scope: "personal", Type: "imessage", Title: "Alex", Source: "chat-guid",
		Provider: "imessage", ProviderID: "chat-guid", CreatedAt: "2026-08-01T09:00:00Z",
		ContentHash: hash, Body: body, Meta: map[string]any{
			"message_count": "1", "message_evidence_schema": 1,
			"message_evidence": []map[string]any{{
				"evidence_ref": id + "#message-guid", "at": "2026-08-01T09:00:00Z",
				"from_me": true, "sender": "Me", "block_start": 14, "block_end": 22,
			}},
		},
	}
	if err := writeMappedMemory(cfg, mm); err != nil {
		t.Fatal(err)
	}
	migrated, err := parseMemory(dest)
	if err != nil {
		t.Fatal(err)
	}
	if migrated.CreatedAt != createdAt || migrated.ContentHash != hash || migrated.Meta["message_evidence"] == nil {
		t.Fatalf("migration changed identity clocks/hash or omitted evidence: %+v", migrated)
	}
	snap := briefSnapshot{Key: "imessage", LastBriefAt: "2026-08-01T08:00:00Z", HashSchemaVersion: briefHashSchemaVersion, Items: map[string]string{id: hash}}
	if delta := classify(snap, []Memory{migrated}, time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)); len(delta.Items) != 0 {
		t.Fatalf("schema-only migration surfaced a Brief update: %+v", delta.Items)
	}

	first, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeMappedMemory(cfg, mm); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("second resync rewrote an already-migrated memory")
	}

	// The projection must be reproducible from the migrated Markdown alone,
	// including after index.db is deleted.
	releaseIngestLeasesOwnedHere(cfg)
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	ref := id + "#message-guid"
	row1, ok, err := gmailSegmentByRef(context.Background(), cfg, id, ref)
	if err != nil || !ok || row1.Text != "Me: same" || row1.At != "2026-08-01T09:00:00Z" || imessageDirection(row1.BlockRefs) != "outgoing" {
		t.Fatalf("rebuilt evidence row: row=%+v ok=%v err=%v", row1, ok, err)
	}
	readPayload := shapeReadMemoryEvidenceRef(cfg, migrated, row1, map[string]any{})
	readReceipt, ok := readPayload["receipt"].(boundedReadReceipt)
	if !ok || readReceipt.EvidenceRef != ref || readReceipt.Sender != "Me" || readReceipt.At != "2026-08-01T09:00:00Z" || readReceipt.Direction != "outgoing" {
		t.Fatalf("read_memory evidence receipt = %#v", readPayload["receipt"])
	}
	db, err := openIndexRO(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	ids, receipts, err := gmailSegmentQueryArm(context.Background(), db, "same", "")
	db.Close()
	if err != nil || len(ids) != 1 || ids[0] != id || receipts[id].EvidenceRef != ref || receipts[id].Direction != "outgoing" || receipts[id].At != "2026-08-01T09:00:00Z" {
		t.Fatalf("segment FTS receipt: ids=%v receipts=%+v err=%v", ids, receipts, err)
	}
	for _, path := range []string{dbPath(cfg), dbPath(cfg) + "-wal", dbPath(cfg) + "-shm"} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	row2, ok, err := gmailSegmentByRef(context.Background(), cfg, id, ref)
	if err != nil || !ok || !reflect.DeepEqual(row1, row2) {
		t.Fatalf("index deletion changed evidence: before=%+v after=%+v ok=%v err=%v", row1, row2, ok, err)
	}
}

func TestIMessageEvidenceDirectionReceipt(t *testing.T) {
	if got := imessageDirection([]string{"bytes:1-2", "from_me:true"}); got != "outgoing" {
		t.Fatalf("outgoing direction = %q", got)
	}
	if got := imessageDirection([]string{"bytes:1-2", "from_me:false"}); got != "incoming" {
		t.Fatalf("incoming direction = %q", got)
	}
}

func TestIMessageAskDirectionalityDistinguishesAuthorship(t *testing.T) {
	m := Memory{ID: "imessage_chat/asks", Type: "imessage", Provider: "imessage", Text: "Me: can you review?\nAlex: can you send it?", Meta: map[string]any{
		"message_evidence": []map[string]any{
			{"evidence_ref": "imessage_chat/asks#mine", "at": "2026-08-24T09:00:00Z", "from_me": true, "sender": "Me", "block_start": 0, "block_end": 19},
			{"evidence_ref": "imessage_chat/asks#theirs", "at": "2026-08-24T09:01:00Z", "from_me": false, "sender": "Alex", "block_start": 20, "block_end": 42},
		},
	}}
	rows, diag := deriveIMessageSegments(m)
	if diag != nil || len(rows) != 2 || imessageDirection(rows[0].BlockRefs) != "outgoing" || imessageDirection(rows[1].BlockRefs) != "incoming" || rows[0].Sender != "Me" || rows[1].Sender != "Alex" {
		t.Fatalf("ask authorship rows=%+v diag=%+v", rows, diag)
	}
}

func TestIMessageGroupAudienceDoesNotBecomeDirectMessage(t *testing.T) {
	m := Memory{ID: "imessage_chat/group", Type: "imessage", Provider: "imessage", Text: "Alex: hello team", Meta: map[string]any{
		"is_group":         true,
		"participants":     []map[string]string{{"handle": "alex", "name": "Alex"}, {"handle": "sam", "name": "Sam"}},
		"message_evidence": []map[string]any{{"evidence_ref": "imessage_chat/group#one", "at": "2026-08-24T09:00:00Z", "from_me": false, "sender": "Alex", "block_start": 0, "block_end": 16}},
	}}
	rows, diag := deriveIMessageSegments(m)
	if diag != nil || len(rows) != 1 {
		t.Fatalf("rows=%+v diag=%+v", rows, diag)
	}
	if rows[0].Sender != "Alex" || len(rows[0].Recipients) != 0 || imessageAudience(rows[0].BlockRefs) != "group" {
		t.Fatalf("group author/audience attribution=%+v", rows[0])
	}
}

func TestIMessageEvidenceMalformedIdentityFailsClosed(t *testing.T) {
	base := Memory{ID: "imessage_chat/chat", Type: "imessage", Provider: "imessage", Text: "Me: hello"}
	for name, entry := range map[string]map[string]any{
		"missing_direction": {"evidence_ref": base.ID + "#a", "at": "2026-08-01T09:00:00Z", "sender": "Me", "block_start": 0, "block_end": 9},
		"invalid_time":      {"evidence_ref": base.ID + "#a", "at": "not-a-time", "from_me": true, "sender": "Me", "block_start": 0, "block_end": 9},
		"empty_sender":      {"evidence_ref": base.ID + "#a", "at": "2026-08-01T09:00:00Z", "from_me": true, "sender": "", "block_start": 0, "block_end": 9},
	} {
		subRun(t, name, func(t *testing.T) {
			m := base
			m.Meta = map[string]any{"message_evidence": []map[string]any{entry}}
			rows, diag := deriveIMessageSegments(m)
			if len(rows) != 0 || diag == nil || diag.Reason != "message_evidence_malformed" {
				t.Fatalf("rows=%+v diag=%+v", rows, diag)
			}
		})
	}
}

func TestIMessageEvidenceMigrationSIGKILLRecoversThroughJournal(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	const id = "imessage_chat/crash-guid"
	dest := filepath.Join(sourcesRoot(cfg), "imessage", osSafeBase(memory.SafeFilename(id))+".md")
	legacyBytes, err := renderMemory(Memory{
		ID: id, Scope: "personal", Type: "imessage", Provider: "imessage", ProviderID: "crash-guid",
		Title: "Crash", Source: "crash-guid", CreatedAt: "2026-07-01T08:00:00Z",
		ContentHash: "same-hash", Text: "Me: crash boundary", Meta: map[string]any{"message_count": "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicio.Write(dest, legacyBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	mm := memory.MappedMemory{
		StableID: id, Scope: "personal", Type: "imessage", Provider: "imessage", ProviderID: "crash-guid",
		Title: "Crash", Source: "crash-guid", CreatedAt: "2026-08-01T09:00:00Z",
		ContentHash: "same-hash", Body: "Me: crash boundary", Meta: map[string]any{
			"message_count": "1", "message_evidence_schema": 1,
			"message_evidence": []map[string]any{{
				"evidence_ref": id + "#message-guid", "at": "2026-08-01T09:00:00Z",
				"from_me": true, "sender": "Me", "block_start": 0, "block_end": 18,
			}},
		},
	}
	crashed := false
	func() {
		defer func() { crashed = recover() != nil }()
		testHookPostConnectorPublish = func() { panic("SIGKILL at migration publish boundary") }
		defer func() { testHookPostConnectorPublish = nil }()
		_ = writeMappedMemory(cfg, mm)
	}()
	if !crashed {
		t.Fatal("migration crash seam did not fire")
	}
	if dirty, _, _, _ := ingestJournalStatus(cfg); !dirty {
		t.Fatal("migration publish crash left the index false-clean")
	}
	_ = os.Remove(ingestLeasePath(cfg, ingestSourceKey("imessage", "")))
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := gmailSegmentByRef(context.Background(), cfg, id, id+"#message-guid"); err != nil || !ok {
		t.Fatalf("recovery rebuild lost migrated evidence: ok=%v err=%v", ok, err)
	}
}
