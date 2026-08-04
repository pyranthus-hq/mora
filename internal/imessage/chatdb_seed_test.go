package imessage

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// seedChat / seedMsg describe rows for a temp chat.db built by seedChatDB. They let
// the deny-list / lookback / rendering tests exercise the REAL SQL + assembly path
// (not a fake Fetcher) against a pure-Go sqlite file.
type seedChat struct {
	rowid        int64
	guid         string
	display      string
	identifier   string
	participants []string // non-self handles → chat_handle_join roster
}

type seedMsg struct {
	chatID    int64
	guid      string
	date      time.Time
	fromMe    bool
	handle    string // sender handle for non-self messages
	text      string
	attrBody  []byte // raw attributedBody BLOB (optional); the modern body location
	itemType  int64
	retracted int64
	assoc     int64  // associated_message_type (≠0 ⇒ tapback)
	attFile   string // attachment filename (optional)
	attMime   string // attachment mime (optional)
}

// seedChatDB writes a temp chat.db with the minimal schema (including the optional
// item_type / date_retracted columns) and returns its path.
func seedChatDB(t *testing.T, chats []seedChat, msgs []seedMsg) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "chat.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open seed db: %v", err)
	}
	defer db.Close()

	stmts := []string{
		`CREATE TABLE chat (ROWID INTEGER PRIMARY KEY, guid TEXT, display_name TEXT, chat_identifier TEXT)`,
		`CREATE TABLE handle (ROWID INTEGER PRIMARY KEY, id TEXT)`,
		`CREATE TABLE chat_handle_join (chat_id INTEGER, handle_id INTEGER)`,
		`CREATE TABLE message (ROWID INTEGER PRIMARY KEY, guid TEXT, date INTEGER, is_from_me INTEGER, text TEXT,
		    attributedBody BLOB, associated_message_type INTEGER, item_type INTEGER, date_retracted INTEGER, handle_id INTEGER)`,
		`CREATE TABLE chat_message_join (chat_id INTEGER, message_id INTEGER)`,
		`CREATE TABLE attachment (ROWID INTEGER PRIMARY KEY, filename TEXT, mime_type TEXT, total_bytes INTEGER)`,
		`CREATE TABLE message_attachment_join (message_id INTEGER, attachment_id INTEGER)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("create schema: %v", err)
		}
	}

	// Intern handles by id → ROWID.
	handleID := map[string]int64{}
	var nextHandle int64
	intern := func(id string) int64 {
		if id == "" {
			return 0
		}
		if r, ok := handleID[id]; ok {
			return r
		}
		nextHandle++
		if _, err := db.Exec(`INSERT INTO handle (ROWID, id) VALUES (?, ?)`, nextHandle, id); err != nil {
			t.Fatalf("insert handle: %v", err)
		}
		handleID[id] = nextHandle
		return nextHandle
	}

	for _, c := range chats {
		if _, err := db.Exec(`INSERT INTO chat (ROWID, guid, display_name, chat_identifier) VALUES (?, ?, ?, ?)`,
			c.rowid, c.guid, nullable(c.display), nullable(c.identifier)); err != nil {
			t.Fatalf("insert chat: %v", err)
		}
		for _, h := range c.participants {
			hid := intern(h)
			if _, err := db.Exec(`INSERT INTO chat_handle_join (chat_id, handle_id) VALUES (?, ?)`, c.rowid, hid); err != nil {
				t.Fatalf("insert chat_handle_join: %v", err)
			}
		}
	}

	var nextMsg, nextAtt int64
	for _, m := range msgs {
		nextMsg++
		guid := m.guid
		if guid == "" {
			guid = fmt.Sprintf("seed-message-%d", nextMsg)
		}
		hid := int64(0)
		if !m.fromMe {
			hid = intern(m.handle)
		}
		fromMe := 0
		if m.fromMe {
			fromMe = 1
		}
		if _, err := db.Exec(`INSERT INTO message (ROWID, guid, date, is_from_me, text, attributedBody, associated_message_type, item_type, date_retracted, handle_id)
		    VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			nextMsg, guid, timeToCocoaNanos(m.date), fromMe, nullable(m.text), nullableBlob(m.attrBody), m.assoc, m.itemType, m.retracted, hid); err != nil {
			t.Fatalf("insert message: %v", err)
		}
		if _, err := db.Exec(`INSERT INTO chat_message_join (chat_id, message_id) VALUES (?, ?)`, m.chatID, nextMsg); err != nil {
			t.Fatalf("insert chat_message_join: %v", err)
		}
		if m.attFile != "" || m.attMime != "" {
			nextAtt++
			if _, err := db.Exec(`INSERT INTO attachment (ROWID, filename, mime_type, total_bytes) VALUES (?, ?, ?, ?)`,
				nextAtt, nullable(m.attFile), nullable(m.attMime), 12345); err != nil {
				t.Fatalf("insert attachment: %v", err)
			}
			if _, err := db.Exec(`INSERT INTO message_attachment_join (message_id, attachment_id) VALUES (?, ?)`, nextMsg, nextAtt); err != nil {
				t.Fatalf("insert message_attachment_join: %v", err)
			}
		}
	}
	return path
}

func nullable(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func nullableBlob(b []byte) interface{} {
	if len(b) == 0 {
		return nil
	}
	return b
}

// fetchAll pages a LiveFetcher fully and returns every Item.
func fetchAll(t *testing.T, f *LiveFetcher, w FetchWindow) []Item {
	t.Helper()
	var items []Item
	cursor := ""
	for {
		page, err := f.FetchPage(KindIMessageChat, w, cursor)
		if err != nil {
			t.Fatalf("FetchPage(%q): %v", cursor, err)
		}
		items = append(items, page.Items...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return items
}

// bodyFor maps an Item's conversation payload to a rendered body with the given
// resolver (the real end-to-end render path).
func bodyFor(t *testing.T, it Item, r *Resolver) string {
	t.Helper()
	c, ok := it.Payload.(convInput)
	if !ok {
		t.Fatalf("item %q has no convInput payload", it.ProviderID)
	}
	mm := mapConversation(c, r, 0)
	return mm.Body
}

// TestSeededLookback proves the SQL lookback bound: with a 90-day-style cutoff the
// older message is excluded; with all-time (Since zero) it is included (IMSG-06/D-06).
func TestSeededLookback(t *testing.T) {
	chats := []seedChat{{rowid: 1, guid: "iMessage;-;+14155551234", identifier: "+14155551234", participants: []string{"+14155551234"}}}
	msgs := []seedMsg{
		{chatID: 1, date: localDate(2026, 1, 1, 9, 0), handle: "+14155551234", text: "ANCIENT message before cutoff"},
		{chatID: 1, date: localDate(2026, 5, 15, 9, 0), fromMe: true, text: "recent message after cutoff"},
	}
	path := seedChatDB(t, chats, msgs)
	r := resolver1to1()

	f, err := NewLiveFetcher(path, DenyList{})
	if err != nil {
		t.Fatalf("NewLiveFetcher: %v", err)
	}
	defer f.Close()

	// Cutoff at 2026-05-01 excludes the ancient message.
	items := fetchAll(t, f, FetchWindow{Since: localDate(2026, 5, 1, 0, 0)})
	if len(items) != 1 {
		t.Fatalf("want 1 conversation, got %d", len(items))
	}
	body := bodyFor(t, items[0], r)
	if strings.Contains(body, "ANCIENT") {
		t.Fatalf("ancient message should be excluded by lookback:\n%s", body)
	}
	if !strings.Contains(body, "recent message") {
		t.Fatalf("recent message missing:\n%s", body)
	}

	// All-time (Since zero) includes the ancient message.
	all := fetchAll(t, f, FetchWindow{})
	if !strings.Contains(bodyFor(t, all[0], r), "ANCIENT") {
		t.Fatalf("all-time should include the ancient message")
	}
}

// TestSeededDenyContact1to1 proves a denied handle skips its 1:1 conversation
// (sole-counterparty rule, D-08), with phone normalization across formatting.
func TestSeededDenyContact1to1(t *testing.T) {
	chats := []seedChat{
		{rowid: 1, guid: "iMessage;-;+14155551234", identifier: "+14155551234", participants: []string{"+14155551234"}},
		{rowid: 2, guid: "iMessage;-;+19998887777", identifier: "+19998887777", participants: []string{"+19998887777"}},
	}
	msgs := []seedMsg{
		{chatID: 1, date: localDate(2026, 5, 10, 9, 0), handle: "+14155551234", text: "from the denied contact"},
		{chatID: 2, date: localDate(2026, 5, 10, 9, 0), handle: "+19998887777", text: "from a kept contact"},
	}
	path := seedChatDB(t, chats, msgs)

	// Deny via a differently-formatted handle to prove normalization.
	f, err := NewLiveFetcher(path, DenyList{Contacts: []string{"+1 (415) 555-1234"}})
	if err != nil {
		t.Fatalf("NewLiveFetcher: %v", err)
	}
	defer f.Close()

	items := fetchAll(t, f, FetchWindow{})
	if len(items) != 1 {
		t.Fatalf("want 1 conversation (denied 1:1 skipped), got %d", len(items))
	}
	if items[0].ProviderID != "iMessage;-;+19998887777" {
		t.Fatalf("kept the wrong conversation: %s", items[0].ProviderID)
	}
}

// TestSeededDenyContactGroupKeptIntact is the LOCKED rule (D-08): a denied handle
// inside a MULTI-party group does NOT drop the group, and the denied handle's
// messages still appear — group exclusion is thread-granularity only.
func TestSeededDenyContactGroupKeptIntact(t *testing.T) {
	chats := []seedChat{{rowid: 1, guid: "iMessage;+;chat777", display: "Launch Crew",
		participants: []string{"+14155551234", "+19998887777"}}}
	msgs := []seedMsg{
		{chatID: 1, date: localDate(2026, 5, 10, 9, 0), handle: "+14155551234", text: "denied member speaking"},
		{chatID: 1, date: localDate(2026, 5, 10, 9, 1), handle: "+19998887777", text: "other member speaking"},
	}
	path := seedChatDB(t, chats, msgs)
	r := resolver1to1()

	f, err := NewLiveFetcher(path, DenyList{Contacts: []string{"+14155551234"}})
	if err != nil {
		t.Fatalf("NewLiveFetcher: %v", err)
	}
	defer f.Close()

	items := fetchAll(t, f, FetchWindow{})
	if len(items) != 1 {
		t.Fatalf("group containing a denied member must be KEPT, got %d items", len(items))
	}
	body := bodyFor(t, items[0], r)
	if !strings.Contains(body, "denied member speaking") {
		t.Fatalf("denied member's messages must remain in the kept group:\n%s", body)
	}
}

// TestSeededDenyConversationTitle proves a conversation whose display name is denied
// is skipped entirely (thread-granularity, D-08), case-insensitively.
func TestSeededDenyConversationTitle(t *testing.T) {
	chats := []seedChat{
		{rowid: 1, guid: "iMessage;+;chat1", display: "Spoilers", participants: []string{"+14155551234", "+19998887777"}},
		{rowid: 2, guid: "iMessage;+;chat2", display: "Work", participants: []string{"+14155551234", "+19998887777"}},
	}
	msgs := []seedMsg{
		{chatID: 1, date: localDate(2026, 5, 10, 9, 0), handle: "+14155551234", text: "secret"},
		{chatID: 2, date: localDate(2026, 5, 10, 9, 0), handle: "+14155551234", text: "standup at 10"},
	}
	path := seedChatDB(t, chats, msgs)

	f, err := NewLiveFetcher(path, DenyList{Conversations: []string{"spoilers"}})
	if err != nil {
		t.Fatalf("NewLiveFetcher: %v", err)
	}
	defer f.Close()

	items := fetchAll(t, f, FetchWindow{})
	if len(items) != 1 || items[0].ProviderID != "iMessage;+;chat2" {
		t.Fatalf("denied-title conversation should be skipped; got %d items", len(items))
	}
}

// TestSeededAttributedBodyDecode is the hermetic (no-FDA) regression guard for the
// Phase 2.1 received-message drop bug. It seeds messages whose body lives ONLY in
// attributedBody (text column NULL) — the exact shape the bug dropped — and asserts
// the decoded text reaches the rendered conversation through the real
// FetchPage→conversationMessages→decodeAttributedBody→mapConversation path. Every
// other seed test forces attributedBody to NULL, so until now NO default-CI test
// exercised the decode call site (chatdb.go) and the original bug shipped green; this
// closes that hole so a future decoder regression fails in CI without Full Disk Access.
func TestSeededAttributedBodyDecode(t *testing.T) {
	chats := []seedChat{{rowid: 1, guid: "iMessage;-;+14155551234", identifier: "+14155551234", participants: []string{"+14155551234"}}}
	msgs := []seedMsg{
		// Received, text NULL, body only in attributedBody — the dropped case.
		{chatID: 1, date: localDate(2026, 5, 20, 9, 0), handle: "+14155551234", attrBody: buildBlob([]byte("decoded from attributedBody"), nil)},
		// Received inline-attachment placeholder (U+FFFC) WITH an attachment: the body
		// strips to empty so only the attachment marker renders — no junk glyph line.
		{chatID: 1, date: localDate(2026, 5, 20, 9, 1), handle: "+14155551234", attrBody: buildBlob([]byte("￼"), nil), attFile: "IMG_1.jpg", attMime: "image/jpeg"},
		// Sent, text NULL, body only in attributedBody — a post-migration sent message.
		{chatID: 1, date: localDate(2026, 5, 20, 9, 2), fromMe: true, attrBody: buildBlob([]byte("also from attributedBody"), nil)},
	}
	path := seedChatDB(t, chats, msgs)
	r := resolver1to1()

	f, err := NewLiveFetcher(path, DenyList{})
	if err != nil {
		t.Fatalf("NewLiveFetcher: %v", err)
	}
	defer f.Close()

	items := fetchAll(t, f, FetchWindow{})
	if len(items) != 1 {
		t.Fatalf("want 1 conversation, got %d", len(items))
	}
	body := bodyFor(t, items[0], r)

	for _, want := range []string{
		"Neil Patel: decoded from attributedBody",
		"Me: also from attributedBody",
		"Neil Patel: [image: IMG_1.jpg]",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q (attributedBody decode regressed):\n%s", want, body)
		}
	}
	if strings.Contains(body, "￼") {
		t.Fatalf("U+FFFC inline-attachment placeholder leaked into the rendered body:\n%s", body)
	}
}

// TestSeededTextColumnPlaceholderStripped proves the U+FFFC inline-attachment
// placeholder is stripped from the body regardless of source — including the text
// column, which bypasses decodeAttributedBody (chatdb.go reads the text column first).
// Real chat.db rows do carry U+FFFC in the text column; without this the raw "￼" glyph
// leaks into the transcript.
func TestSeededTextColumnPlaceholderStripped(t *testing.T) {
	chats := []seedChat{{rowid: 1, guid: "iMessage;-;+14155551234", identifier: "+14155551234", participants: []string{"+14155551234"}}}
	msgs := []seedMsg{
		// Caption in the TEXT column with a leading placeholder + its image.
		{chatID: 1, date: localDate(2026, 5, 20, 9, 0), handle: "+14155551234", text: "￼on my way", attFile: "IMG_2.jpg", attMime: "image/jpeg"},
		// Bare placeholder in the TEXT column with an image: only the marker renders.
		{chatID: 1, date: localDate(2026, 5, 20, 9, 1), fromMe: true, text: "￼", attFile: "IMG_3.jpg", attMime: "image/jpeg"},
	}
	path := seedChatDB(t, chats, msgs)
	r := resolver1to1()

	f, err := NewLiveFetcher(path, DenyList{})
	if err != nil {
		t.Fatalf("NewLiveFetcher: %v", err)
	}
	defer f.Close()

	body := bodyFor(t, fetchAll(t, f, FetchWindow{})[0], r)
	if strings.Contains(body, "￼") {
		t.Fatalf("U+FFFC from the text column leaked into the body:\n%s", body)
	}
	if !strings.Contains(body, "Neil Patel: on my way") {
		t.Fatalf("caption text lost after stripping the placeholder:\n%s", body)
	}
	if !strings.Contains(body, "[image: IMG_2.jpg]") || !strings.Contains(body, "[image: IMG_3.jpg]") {
		t.Fatalf("attachment markers missing:\n%s", body)
	}
}

// TestAttachmentPathThreadedThrough proves the IMSG-07 amendment: the attachment's
// on-disk location (tilde-expanded) travels in Attachment.Path for the wiring
// boundary, while Filename stays the path-free base name used in rendered output.
func TestAttachmentPathThreadedThrough(t *testing.T) {
	chats := []seedChat{{rowid: 1, guid: "iMessage;-;+14155551234", identifier: "+14155551234", participants: []string{"+14155551234"}}}
	msgs := []seedMsg{
		{chatID: 1, date: localDate(2026, 5, 20, 9, 0), handle: "+14155551234", text: "here's the doc",
			attFile: "~/Library/Messages/Attachments/ab/cd/doc.pdf", attMime: "application/pdf"},
	}
	path := seedChatDB(t, chats, msgs)

	f, err := NewLiveFetcher(path, DenyList{})
	if err != nil {
		t.Fatalf("NewLiveFetcher: %v", err)
	}
	defer f.Close()

	items := fetchAll(t, f, FetchWindow{})
	if len(items) != 1 {
		t.Fatalf("want 1 conversation, got %d", len(items))
	}
	c, ok := items[0].Payload.(convInput)
	if !ok {
		t.Fatalf("item has no convInput payload")
	}
	if len(c.attachments) != 1 {
		t.Fatalf("want 1 attachment, got %d", len(c.attachments))
	}
	att := c.attachments[0]
	if att.Filename != "doc.pdf" {
		t.Fatalf("Filename must stay the base name (rendered output is path-free): %q", att.Filename)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("os.UserHomeDir: %v", err)
	}
	want := filepath.Join(home, "Library/Messages/Attachments/ab/cd/doc.pdf")
	if att.Path != want {
		t.Fatalf("Path must carry the expanded on-disk location: got %q want %q", att.Path, want)
	}
}

// TestSeededStructuredRender proves the end-to-end render path off real SQL: resolved
// name, day header, inline attachment marker, tapback filtered, system event italic.
func TestSeededStructuredRender(t *testing.T) {
	chats := []seedChat{{rowid: 1, guid: "iMessage;-;+14155551234", identifier: "+14155551234", participants: []string{"+14155551234"}}}
	msgs := []seedMsg{
		{chatID: 1, date: localDate(2026, 5, 20, 9, 0), itemType: 1, handle: "+14155551234", text: `Neil named the conversation "Demo"`},
		{chatID: 1, date: localDate(2026, 5, 20, 9, 1), handle: "+14155551234", text: "here's the deck", attFile: "deck.pdf", attMime: "application/pdf"},
		{chatID: 1, date: localDate(2026, 5, 20, 9, 2), fromMe: true, text: "thanks", assoc: 2000 /* tapback → filtered */},
		{chatID: 1, date: localDate(2026, 5, 20, 9, 3), fromMe: true, text: "got it"},
	}
	path := seedChatDB(t, chats, msgs)
	r := resolver1to1()

	f, err := NewLiveFetcher(path, DenyList{})
	if err != nil {
		t.Fatalf("NewLiveFetcher: %v", err)
	}
	defer f.Close()

	items := fetchAll(t, f, FetchWindow{})
	if len(items) != 1 {
		t.Fatalf("want 1 conversation, got %d", len(items))
	}
	body := bodyFor(t, items[0], r)

	for _, want := range []string{
		"## 2026-05-20",
		`*Neil named the conversation "Demo"*`,
		"Neil Patel: here's the deck",
		"Neil Patel: [attachment: deck.pdf · application/pdf]",
		"Me: got it",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "Me: thanks") {
		t.Fatalf("tapback should have been filtered out:\n%s", body)
	}
}
