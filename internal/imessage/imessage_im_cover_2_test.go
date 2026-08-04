package imessage

// Coverage-focused tests for chatdb.go (the LiveFetcher read path). Uses the shared
// seedChatDB helper for well-formed fixtures, imMakeDB for structurally-defective
// chat.db schemas, imClosedDB for query-error branches, and the imFake* driver for
// mid-iteration Next errors. All names are TestIm_* / im-prefixed (merge-safety).

import (
	"database/sql/driver"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// TestIm_NewLiveFetcherOpenError proves NewLiveFetcher surfaces an open failure: a
// nonexistent path fails at openChatDB's forced read-only Ping (mode=ro cannot create
// the file) — the same statements a Full-Disk-Access-denied open would hit.
func TestIm_NewLiveFetcherOpenError(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "does-not-exist.db")
	f, err := NewLiveFetcher(bad, DenyList{})
	if err == nil {
		if f != nil {
			f.Close()
		}
		t.Fatal("NewLiveFetcher on a non-database file must return an error")
	}
}

// TestIm_NewLiveFetcherSchemaMissingColumn proves NewLiveFetcher rejects a chat.db
// whose `message` table is missing a required column with a clear, specific error
// (never a cryptic downstream query failure).
func TestIm_NewLiveFetcherSchemaMissingColumn(t *testing.T) {
	// A valid sqlite DB whose message table omits attributedBody (a required column).
	path := imMakeDB(t,
		`CREATE TABLE message (ROWID INTEGER PRIMARY KEY, guid TEXT, date INTEGER, text TEXT,
		    is_from_me INTEGER, handle_id INTEGER, associated_message_type INTEGER)`,
	)
	f, err := NewLiveFetcher(path, DenyList{})
	if err == nil {
		if f != nil {
			f.Close()
		}
		t.Fatal("NewLiveFetcher must reject a message table missing a required column")
	}
	if !strings.Contains(err.Error(), "unsupported chat.db schema") || !strings.Contains(err.Error(), "attributedBody") {
		t.Fatalf("error %q must name the missing column and the unsupported schema", err)
	}
}

// TestIm_NewLiveFetcherNoMessageTable proves the "no message table" probe branch: a
// valid DB with no `message` table at all is rejected with a specific error.
func TestIm_NewLiveFetcherNoMessageTable(t *testing.T) {
	path := imMakeDB(t, `CREATE TABLE unrelated (a TEXT)`)
	f, err := NewLiveFetcher(path, DenyList{})
	if err == nil {
		if f != nil {
			f.Close()
		}
		t.Fatal("NewLiveFetcher must reject a chat.db with no message table")
	}
	if !strings.Contains(err.Error(), "no `message` table") {
		t.Fatalf("error %q must report the missing message table", err)
	}
}

// TestIm_ProbeMessageSchemaQueryError proves the probe surfaces a wrapped error when
// the PRAGMA query itself fails (closed DB), rather than returning a bogus column set.
func TestIm_ProbeMessageSchemaQueryError(t *testing.T) {
	_, err := probeMessageSchema(imClosedDB(t))
	if err == nil {
		t.Fatal("probeMessageSchema on a closed DB must return an error")
	}
	if !strings.Contains(err.Error(), "probe chat.db schema") {
		t.Fatalf("error %q must wrap the probe context", err)
	}
}

// TestIm_ProbeMessageSchemaScanError proves a PRAGMA row that fails to scan surfaces a
// wrapped probe error (fake driver emits a non-conforming table_info row).
func TestIm_ProbeMessageSchemaScanError(t *testing.T) {
	db := imOpenFakeDB(t, &imFakeBehavior{cols: imPragmaCols, rows: imBadPragmaRow()})
	_, err := probeMessageSchema(db)
	if err == nil || !strings.Contains(err.Error(), "probe chat.db schema") {
		t.Fatalf("probeMessageSchema scan error = %v, want a wrapped probe error", err)
	}
}

// TestIm_ProbeMessageSchemaRowsErr proves a mid-iteration PRAGMA failure (rows.Err)
// surfaces a wrapped probe error (fake driver returns an error from Next).
func TestIm_ProbeMessageSchemaRowsErr(t *testing.T) {
	db := imOpenFakeDB(t, &imFakeBehavior{cols: imPragmaCols, nextErr: fmt.Errorf("im iteration boom")})
	_, err := probeMessageSchema(db)
	if err == nil || !strings.Contains(err.Error(), "probe chat.db schema") {
		t.Fatalf("probeMessageSchema rows.Err = %v, want a wrapped probe error", err)
	}
}

// TestIm_CloseNilDB proves Close is safe on a fetcher whose db handle is nil.
func TestIm_CloseNilDB(t *testing.T) {
	if err := (&LiveFetcher{}).Close(); err != nil {
		t.Fatalf("Close on a nil-db fetcher = %v, want nil", err)
	}
}

// TestIm_FetchPageWrongKind proves FetchPage rejects any kind other than the iMessage
// chat kind.
func TestIm_FetchPageWrongKind(t *testing.T) {
	f := imNewFetcher(t, []seedChat{{rowid: 1, guid: "g", identifier: "+14155551234", participants: []string{"+14155551234"}}}, nil)
	defer f.Close()
	if _, err := f.FetchPage(ItemKind("not_imessage"), FetchWindow{}, ""); err == nil {
		t.Fatal("FetchPage with a wrong kind must return an error")
	}
}

// TestIm_FetchPageBadCursor proves a non-numeric cursor is rejected with a clear error.
func TestIm_FetchPageBadCursor(t *testing.T) {
	f := imNewFetcher(t, []seedChat{{rowid: 1, guid: "g", identifier: "+14155551234", participants: []string{"+14155551234"}}}, nil)
	defer f.Close()
	if _, err := f.FetchPage(KindIMessageChat, FetchWindow{}, "not-a-number"); err == nil {
		t.Fatal("FetchPage with a non-numeric cursor must return an error")
	}
}

// TestIm_FetchPagePaginatesAcrossPages proves the page cursor: with 51 conversations
// the first page returns exactly chatPageSize items plus a non-empty cursor, and the
// second page (driven by that numeric cursor) returns the remainder and an empty
// cursor. This exercises the cursor-set branch and the numeric-cursor scan.
func TestIm_FetchPagePaginatesAcrossPages(t *testing.T) {
	const total = 51 // chatPageSize (50) + 1 → forces a second page
	var chats []seedChat
	var msgs []seedMsg
	for i := 1; i <= total; i++ {
		h := fmt.Sprintf("+1415555%04d", i)
		chats = append(chats, seedChat{rowid: int64(i), guid: fmt.Sprintf("g%d", i), identifier: h, participants: []string{h}})
		msgs = append(msgs, seedMsg{chatID: int64(i), date: localDate(2026, 5, 20, 9, 0), handle: h, text: "hi"})
	}
	f := imNewFetcher(t, chats, msgs)
	defer f.Close()

	page1, err := f.FetchPage(KindIMessageChat, FetchWindow{}, "")
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if len(page1.Items) != 50 {
		t.Fatalf("page 1 items = %d, want 50 (one full page)", len(page1.Items))
	}
	if page1.NextCursor == "" {
		t.Fatal("page 1 must return a non-empty cursor when a full page is returned")
	}

	page2, err := f.FetchPage(KindIMessageChat, FetchWindow{}, page1.NextCursor)
	if err != nil {
		t.Fatalf("page 2 (cursor %q): %v", page1.NextCursor, err)
	}
	if len(page2.Items) != 1 {
		t.Fatalf("page 2 items = %d, want 1 (the remainder)", len(page2.Items))
	}
	if page2.NextCursor != "" {
		t.Fatalf("page 2 cursor = %q, want empty (last page)", page2.NextCursor)
	}
}

// TestIm_FetchPageChatQueryError proves a failure listing chats (closed DB) surfaces a
// wrapped "list chats" error.
func TestIm_FetchPageChatQueryError(t *testing.T) {
	f := &LiveFetcher{db: imClosedDB(t)}
	_, err := f.FetchPage(KindIMessageChat, FetchWindow{}, "")
	if err == nil || !strings.Contains(err.Error(), "list chats") {
		t.Fatalf("FetchPage chat-query error = %v, want a wrapped list-chats error", err)
	}
}

// TestIm_FetchPageChatScanError proves a chat row that fails to scan surfaces a
// wrapped "scan chat" error. The fake driver emits a chat row whose ROWID value
// ("x") cannot convert to the int64 scan target — a corruption a real sqlite chat
// table (ROWID is always an integer) cannot produce.
func TestIm_FetchPageChatScanError(t *testing.T) {
	f := &LiveFetcher{db: imOpenFakeDB(t, &imFakeBehavior{
		cols: []string{"ROWID", "guid", "display_name", "chat_identifier"},
		rows: [][]driver.Value{{"x", "g", nil, nil}},
	})}
	_, err := f.FetchPage(KindIMessageChat, FetchWindow{}, "")
	if err == nil || !strings.Contains(err.Error(), "scan chat") {
		t.Fatalf("FetchPage chat-scan error = %v, want a wrapped scan-chat error", err)
	}
}

// TestIm_FetchPageChatRowsErr proves a mid-iteration failure while listing chats
// surfaces a wrapped "iterate chats" error (fake driver errors from Next).
func TestIm_FetchPageChatRowsErr(t *testing.T) {
	f := &LiveFetcher{db: imOpenFakeDB(t, &imFakeBehavior{
		cols:    []string{"ROWID", "guid", "display_name", "chat_identifier"},
		nextErr: fmt.Errorf("im chat iteration boom"),
	})}
	_, err := f.FetchPage(KindIMessageChat, FetchWindow{}, "")
	if err == nil || !strings.Contains(err.Error(), "iterate chats") {
		t.Fatalf("FetchPage chat rows.Err = %v, want a wrapped iterate-chats error", err)
	}
}

// TestIm_FetchPageSkipsConversationOnParticipantError proves a per-conversation
// failure (here: the roster tables are absent, so chatParticipants errors) is skipped,
// never aborting the whole page — the honest-snapshot behavior.
func TestIm_FetchPageSkipsConversationOnParticipantError(t *testing.T) {
	// message table is complete (NewLiveFetcher probe passes) but chat_handle_join /
	// handle are absent → chatParticipants' JOIN query fails per-conversation.
	path := imMakeDB(t,
		`CREATE TABLE chat (ROWID INTEGER PRIMARY KEY, guid TEXT, display_name TEXT, chat_identifier TEXT)`,
		imMessageDDL,
		`CREATE TABLE chat_message_join (chat_id INTEGER, message_id INTEGER)`,
		`INSERT INTO chat VALUES (1, 'g1', NULL, '+14155551234')`,
	)
	f, err := NewLiveFetcher(path, DenyList{})
	if err != nil {
		t.Fatalf("NewLiveFetcher: %v", err)
	}
	defer f.Close()
	page, err := f.FetchPage(KindIMessageChat, FetchWindow{}, "")
	if err != nil {
		t.Fatalf("FetchPage must not abort on a per-conversation error: %v", err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("conversation with an unreadable roster should be skipped, got %d items", len(page.Items))
	}
}

// TestIm_FetchPageSkipsConversationOnMessageError proves a conversation whose message
// query fails (here: the attachment join tables are absent) is skipped rather than
// aborting the page.
func TestIm_FetchPageSkipsConversationOnMessageError(t *testing.T) {
	// Roster tables present (chatParticipants succeeds) but attachment join tables
	// absent → the conversation message query fails per-conversation.
	path := imMakeDB(t,
		`CREATE TABLE chat (ROWID INTEGER PRIMARY KEY, guid TEXT, display_name TEXT, chat_identifier TEXT)`,
		`CREATE TABLE handle (ROWID INTEGER PRIMARY KEY, id TEXT)`,
		`CREATE TABLE chat_handle_join (chat_id INTEGER, handle_id INTEGER)`,
		imMessageDDL,
		`CREATE TABLE chat_message_join (chat_id INTEGER, message_id INTEGER)`,
		`INSERT INTO chat VALUES (1, 'g1', NULL, '+14155551234')`,
		`INSERT INTO handle VALUES (1, '+14155551234')`,
		`INSERT INTO chat_handle_join VALUES (1, 1)`,
	)
	f, err := NewLiveFetcher(path, DenyList{})
	if err != nil {
		t.Fatalf("NewLiveFetcher: %v", err)
	}
	defer f.Close()
	page, err := f.FetchPage(KindIMessageChat, FetchWindow{}, "")
	if err != nil {
		t.Fatalf("FetchPage must not abort on a per-conversation message error: %v", err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("conversation with an unreadable message query should be skipped, got %d items", len(page.Items))
	}
}

// TestIm_ConversationMessagesRowsErr proves a mid-iteration failure reading a
// conversation's messages surfaces a wrapped "iterate conversation" error.
func TestIm_ConversationMessagesRowsErr(t *testing.T) {
	f := &LiveFetcher{db: imOpenFakeDB(t, &imFakeBehavior{
		cols:    imMessageQueryCols,
		nextErr: fmt.Errorf("im message iteration boom"),
	})}
	_, _, _, err := f.conversationMessages(1, 0)
	if err == nil || !strings.Contains(err.Error(), "iterate conversation") {
		t.Fatalf("conversationMessages rows.Err = %v, want a wrapped iterate-conversation error", err)
	}
}

// TestIm_ConversationMessagesScanErrorTolerated proves an anomalous message row (a
// non-integer date that fails to scan) is skipped while well-formed messages in the
// same conversation still render.
func TestIm_ConversationMessagesScanErrorTolerated(t *testing.T) {
	goodNanos := timeToCocoaNanos(localDate(2026, 5, 20, 9, 0))
	path := imMakeDB(t,
		`CREATE TABLE chat (ROWID INTEGER PRIMARY KEY, guid TEXT, display_name TEXT, chat_identifier TEXT)`,
		`CREATE TABLE handle (ROWID INTEGER PRIMARY KEY, id TEXT)`,
		`CREATE TABLE chat_handle_join (chat_id INTEGER, handle_id INTEGER)`,
		// Untyped `date` column so we can insert a non-integer value that fails scanning.
		`CREATE TABLE message (ROWID INTEGER PRIMARY KEY, guid TEXT, date, is_from_me INTEGER, text TEXT,
		    attributedBody BLOB, associated_message_type INTEGER, item_type INTEGER, date_retracted INTEGER, handle_id INTEGER)`,
		`CREATE TABLE chat_message_join (chat_id INTEGER, message_id INTEGER)`,
		`CREATE TABLE attachment (ROWID INTEGER PRIMARY KEY, filename TEXT, mime_type TEXT, total_bytes INTEGER)`,
		`CREATE TABLE message_attachment_join (message_id INTEGER, attachment_id INTEGER)`,
		`INSERT INTO chat VALUES (1, 'g1', NULL, '+14155551234')`,
		`INSERT INTO handle VALUES (1, '+14155551234')`,
		`INSERT INTO chat_handle_join VALUES (1, 1)`,
		// Bad row: date is text → Scan(&date int64) fails → row skipped.
		`INSERT INTO message (ROWID, guid, date, is_from_me, text, associated_message_type, item_type, date_retracted, handle_id)
		    VALUES (1, 'm1', 'not-a-number', 0, 'BAD ROW should be skipped', 0, 0, 0, 1)`,
		fmt.Sprintf(`INSERT INTO message (ROWID, guid, date, is_from_me, text, associated_message_type, item_type, date_retracted, handle_id)
		    VALUES (2, 'm2', %d, 0, 'GOOD ROW survives', 0, 0, 0, 1)`, goodNanos),
		`INSERT INTO chat_message_join VALUES (1, 1), (1, 2)`,
	)
	f, err := NewLiveFetcher(path, DenyList{})
	if err != nil {
		t.Fatalf("NewLiveFetcher: %v", err)
	}
	defer f.Close()
	items := fetchAll(t, f, FetchWindow{})
	if len(items) != 1 {
		t.Fatalf("want 1 conversation, got %d", len(items))
	}
	body := bodyFor(t, items[0], resolver1to1())
	if !strings.Contains(body, "GOOD ROW survives") {
		t.Fatalf("well-formed message dropped:\n%s", body)
	}
	if strings.Contains(body, "BAD ROW should be skipped") {
		t.Fatalf("anomalous message should have been skipped:\n%s", body)
	}
}

// TestIm_ConversationTapbackOnlyProducesNoItem proves a conversation whose only
// message is a filtered tapback (associated_message_type != 0) yields no memory
// (nothing renderable in the window).
func TestIm_ConversationTapbackOnlyProducesNoItem(t *testing.T) {
	f := imNewFetcher(t,
		[]seedChat{{rowid: 1, guid: "g1", identifier: "+14155551234", participants: []string{"+14155551234"}}},
		[]seedMsg{{chatID: 1, date: localDate(2026, 5, 20, 9, 0), handle: "+14155551234", text: "Loved this", assoc: 2000}},
	)
	defer f.Close()
	if items := fetchAll(t, f, FetchWindow{}); len(items) != 0 {
		t.Fatalf("tapback-only conversation should produce no item, got %d", len(items))
	}
}

// TestIm_ConversationEmptySystemRowSkipped proves a system row with no text renders
// nothing (skipped in the keep-switch) while a normal message in the same
// conversation is kept.
func TestIm_ConversationEmptySystemRowSkipped(t *testing.T) {
	f := imNewFetcher(t,
		[]seedChat{{rowid: 1, guid: "g1", identifier: "+14155551234", participants: []string{"+14155551234"}}},
		[]seedMsg{
			// Empty system event (item_type set, no text/attachment) → nothing to render.
			{chatID: 1, date: localDate(2026, 5, 20, 9, 0), handle: "+14155551234", itemType: 1},
			{chatID: 1, date: localDate(2026, 5, 20, 9, 1), fromMe: true, text: "got it"},
		},
	)
	defer f.Close()
	items := fetchAll(t, f, FetchWindow{})
	if len(items) != 1 {
		t.Fatalf("want 1 conversation, got %d", len(items))
	}
	body := bodyFor(t, items[0], resolver1to1())
	if !strings.Contains(body, "Me: got it") {
		t.Fatalf("normal message missing:\n%s", body)
	}
	// The empty system row contributes no line; only the single real message is present.
	if n := strings.Count(body, ": "); n != 1 {
		t.Fatalf("empty system row should render nothing; want exactly 1 message line, body:\n%s", body)
	}
}

// TestIm_ConversationRetractedAndMimeOnlyAttachment proves two conversationMessages
// classification branches at the SQL layer: a retracted row (date_retracted != 0)
// renders as the removed marker, and an attachment row with a MIME type but no
// filename still renders a metadata marker.
func TestIm_ConversationRetractedAndMimeOnlyAttachment(t *testing.T) {
	f := imNewFetcher(t,
		[]seedChat{{rowid: 1, guid: "g1", identifier: "+14155551234", participants: []string{"+14155551234"}}},
		[]seedMsg{
			{chatID: 1, date: localDate(2026, 5, 20, 9, 0), handle: "+14155551234", retracted: 999_000},
			// Attachment with a MIME type but NO filename → the mime-only marker branch.
			{chatID: 1, date: localDate(2026, 5, 20, 9, 1), handle: "+14155551234", text: "look", attMime: "image/png"},
		},
	)
	defer f.Close()
	items := fetchAll(t, f, FetchWindow{})
	if len(items) != 1 {
		t.Fatalf("want 1 conversation, got %d", len(items))
	}
	body := bodyFor(t, items[0], resolver1to1())
	if !strings.Contains(body, "Neil Patel: [message removed]") {
		t.Fatalf("retracted message not rendered as the removed marker:\n%s", body)
	}
	if !strings.Contains(body, "[image: image/png]") {
		t.Fatalf("mime-only attachment marker missing:\n%s", body)
	}
}

// TestIm_DenyByChatIdentifier proves a conversation is skipped when its chat_identifier
// (not its display name) matches the deny-list (case-insensitive).
func TestIm_DenyByChatIdentifier(t *testing.T) {
	f := imNewFetcherDeny(t,
		[]seedChat{{rowid: 1, guid: "g1", identifier: "Spoilers", participants: []string{"+14155551234", "+19998887777"}}},
		[]seedMsg{{chatID: 1, date: localDate(2026, 5, 20, 9, 0), handle: "+14155551234", text: "secret"}},
		DenyList{Conversations: []string{"spoilers"}},
	)
	defer f.Close()
	if items := fetchAll(t, f, FetchWindow{}); len(items) != 0 {
		t.Fatalf("conversation denied by chat_identifier should be skipped, got %d", len(items))
	}
}

// TestIm_DenyEmptyRosterByIdentifier proves the sole-counterparty rule when the roster
// is empty: a 1:1 with no chat_handle_join rows falls back to its identifier as the
// denied handle and is skipped.
func TestIm_DenyEmptyRosterByIdentifier(t *testing.T) {
	f := imNewFetcherDeny(t,
		// No participants → empty roster; identifier is the sole-counterparty handle.
		[]seedChat{{rowid: 1, guid: "g1", identifier: "+14155551234"}},
		[]seedMsg{{chatID: 1, date: localDate(2026, 5, 20, 9, 0), handle: "+14155551234", text: "hi"}},
		DenyList{Contacts: []string{"+1 (415) 555-1234"}}, // differently formatted → normalization
	)
	defer f.Close()
	if items := fetchAll(t, f, FetchWindow{}); len(items) != 0 {
		t.Fatalf("empty-roster 1:1 whose identifier is denied should be skipped, got %d", len(items))
	}
}

// ---------------------------------------------------------------------------
// chatdb test helpers (im-prefixed)
// ---------------------------------------------------------------------------

// imMessageDDL is a complete `message` table (all required + optional columns) for raw
// chat.db fixtures whose defect lies elsewhere (missing roster/attachment tables).
const imMessageDDL = `CREATE TABLE message (ROWID INTEGER PRIMARY KEY, guid TEXT, date INTEGER, is_from_me INTEGER, text TEXT,
    attributedBody BLOB, associated_message_type INTEGER, item_type INTEGER, date_retracted INTEGER, handle_id INTEGER)`

// imMessageQueryCols is a 13-name placeholder matching the arity of the conversation
// message SELECT (used only when the fake driver returns zero rows before erroring).
var imMessageQueryCols = []string{"ROWID", "guid", "date", "is_from_me", "text", "attributedBody",
	"associated_message_type", "item_type", "date_retracted", "id", "filename", "mime_type", "total_bytes"}

// imNewFetcher seeds a well-formed chat.db and opens a LiveFetcher with no deny list.
func imNewFetcher(t *testing.T, chats []seedChat, msgs []seedMsg) *LiveFetcher {
	t.Helper()
	return imNewFetcherDeny(t, chats, msgs, DenyList{})
}

// imNewFetcherDeny seeds a well-formed chat.db and opens a LiveFetcher with the given
// deny list.
func imNewFetcherDeny(t *testing.T, chats []seedChat, msgs []seedMsg, deny DenyList) *LiveFetcher {
	t.Helper()
	f, err := NewLiveFetcher(seedChatDB(t, chats, msgs), deny)
	if err != nil {
		t.Fatalf("NewLiveFetcher: %v", err)
	}
	return f
}
