package segments

import (
	"context"
	"database/sql"
	"github.com/pyranthus-hq/mora/internal/memory"
	_ "modernc.org/sqlite"
	"path/filepath"
	"reflect"
	"testing"
)

func gmail(id, text string, messages []map[string]any) memory.Memory {
	return memory.Memory{ID: id, Provider: "gmail", Type: "email", Text: text, Meta: map[string]any{"messages": messages}}
}
func TestDeriveGmailSegmentsFailClosedPriorityAndRows(t *testing.T) {
	body := "From: A <A@EXAMPLE.COM>\n\nfirst" + BodySeparator + "From: b@example.com\n\nsecond"
	messages := []map[string]any{{"message_ref": "gmail_thread/x#a", "sender": "a@example.com", "to": []string{" Z@x.com ", "z@x.com"}, "cc": []string{"a@x.com"}, "at": "2026-01-01T00:00:00Z", "block_refs": []string{"body"}}, {"message_ref": "gmail_thread/x#b", "sender": "b@example.com"}}
	m := gmail("gmail_thread/x", body, messages)
	rows, diag := Derive(m)
	if diag != nil || len(rows) != 2 || rows[0].Text != "first" || !reflect.DeepEqual(rows[0].Recipients, []string{"a@x.com", "z@x.com"}) {
		t.Fatalf("rows=%+v diag=%+v", rows, diag)
	}
	m.Truncated = true
	if _, d := Derive(m); d == nil || d.Reason != DiagTruncated {
		t.Fatalf("d=%+v", d)
	}
	m.Truncated = false
	m.Text = "one"
	if _, d := Derive(m); d == nil || d.Reason != DiagCountMismatch {
		t.Fatalf("d=%+v", d)
	}
	m.Text = body
	m.Meta["messages"] = []map[string]any{{"message_ref": "gmail_thread/x#a", "sender": "wrong"}, {"message_ref": "gmail_thread/x#b", "sender": "b@example.com"}}
	if _, d := Derive(m); d == nil || d.Reason != DiagOrderingMismatch {
		t.Fatalf("d=%+v", d)
	}
	m.Meta["messages"] = []map[string]any{{"message_ref": "other#a", "sender": "a@example.com"}, {"message_ref": "gmail_thread/x#b", "sender": "b@example.com"}}
	if _, d := Derive(m); d == nil || d.Reason != DiagMalformedRef {
		t.Fatalf("d=%+v", d)
	}
	m.Meta["messages"] = []map[string]any{{"message_ref": "gmail_thread/x#a", "sender": "a@example.com"}, {"message_ref": "gmail_thread/x#a", "sender": "b@example.com"}}
	if _, d := Derive(m); d == nil || d.Reason != DiagDuplicateRef {
		t.Fatalf("d=%+v", d)
	}
}
func TestDeriveIgnoresUnclaimedShapes(t *testing.T) {
	for _, m := range []memory.Memory{{Provider: "calendar"}, {Provider: "gmail", Meta: map[string]any{}}, {ProviderID: "gmail/work", Meta: map[string]any{}}} {
		r, d := Derive(m)
		if r != nil || d != nil {
			t.Fatalf("m=%+v rows=%v diag=%v", m, r, d)
		}
	}
}
func TestDeriveIMessageSegmentsAndLegacyCoverage(t *testing.T) {
	legacy := memory.Memory{ID: "imessage_chat/chat", Type: "imessage", Provider: "imessage", Text: "Me: legacy"}
	rows, diag := Derive(legacy)
	if len(rows) != 0 || diag == nil || diag.Reason != "message_evidence_unavailable" {
		t.Fatalf("rows=%v diag=%+v", rows, diag)
	}
	body := "## 2026-08-01\nMe: same\nAlex: same"
	yes, no := true, false
	m := legacy
	m.Text = body
	m.Meta = map[string]any{"message_evidence": []map[string]any{{"evidence_ref": m.ID + "#a", "at": "2026-08-01T09:00:00Z", "from_me": yes, "sender": "Me", "block_start": 14, "block_end": 22}, {"evidence_ref": m.ID + "#b", "at": "2026-08-01T09:01:00Z", "from_me": no, "sender": "Alex", "block_start": 23, "block_end": 33}}}
	rows, diag = Derive(m)
	if diag != nil || len(rows) != 2 || rows[0].Text != "Me: same" || rows[1].Text != "Alex: same" || Direction(rows[0].BlockRefs) != "outgoing" || Direction(rows[1].BlockRefs) != "incoming" {
		t.Fatalf("rows=%+v diag=%+v", rows, diag)
	}
}
func TestIMessageMalformedEvidenceFailsClosed(t *testing.T) {
	base := memory.Memory{ID: "imessage_chat/x", Provider: "imessage", Text: "hello"}
	bad := []any{nil, []map[string]any{}, []map[string]any{{"evidence_ref": "bad", "at": "bad", "sender": "", "block_start": -1, "block_end": 9}}}
	for _, raw := range bad {
		m := base
		m.Meta = map[string]any{"message_evidence": raw}
		if rows, d := Derive(m); rows != nil || d == nil || d.Reason != "message_evidence_malformed" {
			t.Fatalf("raw=%v rows=%v d=%+v", raw, rows, d)
		}
	}
	if Direction(nil) != "" {
		t.Fatal("direction")
	}
}
func TestBlockSenderAndRecipientNormalization(t *testing.T) {
	if got, ok := BlockSender("From: Name <A@Example.COM>\n\nbody"); !ok || got != "a@example.com" {
		t.Fatalf("got=%q ok=%v", got, ok)
	}
	if _, ok := BlockSender("body"); ok {
		t.Fatal("missing header")
	}
	if got, ok := BlockSender("From: not an address"); !ok || got != "not an address" {
		t.Fatalf("fallback=%q %v", got, ok)
	}
	if got := MergeRecipients([]string{" B@x ", "a@x"}, []string{"b@x", ""}); !reflect.DeepEqual(got, []string{"a@x", "b@x"}) {
		t.Fatalf("got=%v", got)
	}
}
func openDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
func TestStoreWriteLookupDiagnosticAndClear(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range SchemaStatements {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	prepared, err := Prepare(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	m := gmail("gmail_thread/x", "From: a@example.com\n\nhello", []map[string]any{{"message_ref": "gmail_thread/x#a", "sender": "a@example.com", "to": []string{"b@example.com"}, "block_refs": []string{"body"}}})
	if err := prepared.Write(ctx, m); err != nil {
		t.Fatal(err)
	}
	bad := m
	bad.ID = "gmail_thread/bad"
	bad.Truncated = true
	if err := prepared.Write(ctx, bad); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	row, ok, err := Lookup(ctx, db, m.ID, m.ID+"#a")
	if err != nil || !ok || row.Text != "hello" || Direction(row.BlockRefs) != "" {
		t.Fatalf("row=%+v ok=%v err=%v", row, ok, err)
	}
	if _, ok, err := Lookup(ctx, db, m.ID, "missing"); err != nil || ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	var reason string
	if err := db.QueryRow(`SELECT reason FROM gmail_segment_diagnostics WHERE memory_id=?`, bad.ID).Scan(&reason); err != nil || reason != DiagTruncated {
		t.Fatalf("reason=%q err=%v", reason, err)
	}
	tx, err = db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := Clear(ctx, tx, m.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := Lookup(ctx, db, m.ID, m.ID+"#a"); err != nil || ok {
		t.Fatalf("after clear ok=%v err=%v", ok, err)
	}
}
func TestStatementsNilClose(t *testing.T) { var s *Statements; s.Close() }
