package whatsapp

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
	_ "modernc.org/sqlite"
)

func seedDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ChatStorage.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	statements := []string{
		`CREATE TABLE ZWACHATSESSION (Z_PK INTEGER PRIMARY KEY, ZCONTACTJID TEXT, ZCONTACTIDENTIFIER TEXT, ZPARTNERNAME TEXT)`,
		`CREATE TABLE ZWAGROUPMEMBER (Z_PK INTEGER PRIMARY KEY, ZCONTACTNAME TEXT, ZFIRSTNAME TEXT, ZMEMBERJID TEXT)`,
		`CREATE TABLE ZWAMEDIAITEM (Z_PK INTEGER PRIMARY KEY, ZTITLE TEXT, ZVCARDNAME TEXT, ZLATITUDE REAL, ZLONGITUDE REAL)`,
		`CREATE TABLE ZWAMESSAGE (Z_PK INTEGER PRIMARY KEY, ZCHATSESSION INTEGER, ZGROUPMEMBER INTEGER, ZMEDIAITEM INTEGER, ZISFROMME INTEGER, ZMESSAGETYPE INTEGER, ZMESSAGEDATE REAL, ZPUSHNAME TEXT, ZSTANZAID TEXT, ZTEXT TEXT, ZFROMJID TEXT)`,
		`INSERT INTO ZWACHATSESSION VALUES (1,'family@g.us','','Family')`,
		`INSERT INTO ZWACHATSESSION VALUES (2,'15551234567@s.whatsapp.net','','Riya')`,
		`INSERT INTO ZWAGROUPMEMBER VALUES (1,'Asha','','asha@s.whatsapp.net')`,
		`INSERT INTO ZWAMEDIAITEM VALUES (1,'','',0,0)`,
		`INSERT INTO ZWAMEDIAITEM VALUES (2,'','Nikhil',0,0)`,
		`INSERT INTO ZWAMEDIAITEM VALUES (3,'Map pin','',37.7749,-122.4194)`,
		`INSERT INTO ZWAMESSAGE VALUES (1,1,1,1,0,1,800000000,'fallback','group-1','','asha@s.whatsapp.net')`,
		`INSERT INTO ZWAMESSAGE VALUES (2,1,1,2,0,5,800000001,'fallback','group-2','','asha@s.whatsapp.net')`,
		`INSERT INTO ZWAMESSAGE VALUES (3,1,1,3,0,4,800000002,'fallback','group-3','','asha@s.whatsapp.net')`,
		`INSERT INTO ZWAMESSAGE VALUES (6,1,NULL,NULL,1,0,800000002.5,'','group-4','Got it','')`,
		`INSERT INTO ZWAMESSAGE VALUES (4,2,NULL,NULL,0,0,800000003,'Riya','dm-1','Can you send this tomorrow?','riya@s.whatsapp.net')`,
		`INSERT INTO ZWAMESSAGE VALUES (5,2,NULL,NULL,1,2,800000004,'','dm-2','','')`,
	}
	for _, stmt := range statements {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	return path
}

func TestSeedFetchExercisesRealQueryAndDecode(t *testing.T) {
	f, err := NewLiveFetcher(seedDB(t))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	page, err := f.FetchPage(KindConversation, memory.FetchWindow{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("got %d conversations", len(page.Items))
	}
	group := MapConversationFn()(page.Items[0], "personal", 0)
	if group.Provider != "whatsapp" || group.StableID != "whatsapp_conversation/family@g.us" {
		t.Fatalf("bad identity: %+v", group)
	}
	if group.Meta["relevance_lane"] != "intelligence" || group.Meta["chat_kind"] != "group" {
		t.Fatalf("bad gate metadata: %#v", group.Meta)
	}
	for _, want := range []string{"Asha: [image]", "Asha: [contact: Nikhil]", "Asha: [location: 37.774900, -122.419400]"} {
		if !strings.Contains(group.Body, want) {
			t.Errorf("body missing %q:\n%s", want, group.Body)
		}
	}
	dm := MapConversationFn()(page.Items[1], "personal", 0)
	if dm.Meta["relevance_lane"] != "personal_action" {
		t.Fatalf("dm lane: %#v", dm.Meta)
	}
	if !strings.Contains(dm.Body, "Riya: Can you send this tomorrow?") || !strings.Contains(dm.Body, "Me: [voice note]") {
		t.Fatalf("dm body:\n%s", dm.Body)
	}
	if dm.CreatedAt == "" {
		t.Fatal("newest message did not anchor created_at")
	}
}

func TestBusyGroupWithoutOwnerParticipationNeverGetsPriority(t *testing.T) {
	messages := []message{
		{id: "1", sender: "A", body: "Here is a very substantive update about the project and its deadline."},
		{id: "2", sender: "B", body: "Another detailed response that would otherwise pass the content gate."},
	}
	if ownerParticipatedInGroup(messages) {
		t.Fatal("incoming volume was mistaken for owner participation")
	}
	conv := conversation{jid: "busy@g.us", title: "Busy group", group: true, messages: messages}
	mm := MapConversationFn()(memory.Item{Kind: KindConversation, ProviderID: conv.jid, Payload: conv}, "global", 0)
	if mm.Meta["relevance_lane"] != "none" || mm.Meta["owner_participated"] != false {
		t.Fatalf("inactive group was prioritized: %#v", mm.Meta)
	}
	if !strings.Contains(mm.Meta["inclusion_rationale"].(string), "volume alone never earns") {
		t.Fatalf("rationale does not explain volume guard: %#v", mm.Meta)
	}
}

func TestInvertedTruncationKeepsNewest(t *testing.T) {
	conv := conversation{jid: "x@s.whatsapp.net", title: "X", messages: []message{
		{id: "1", at: time.Unix(1, 0), sender: "X", body: strings.Repeat("old ", 30)},
		{id: "2", at: time.Unix(2, 0), fromMe: true, body: "new"},
	}}
	mm := MapConversationFn()(memory.Item{Kind: KindConversation, ProviderID: conv.jid, Payload: conv}, "global", 45)
	if !mm.Truncated || strings.Contains(mm.Body, "old old old") || !strings.Contains(mm.Body, "Me: new") {
		t.Fatalf("bad inverted truncation: %q", mm.Body)
	}
}

func TestUnsupportedSchemaFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ChatStorage.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE ZWACHATSESSION (Z_PK INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	_, err = NewLiveFetcher(path)
	if err == nil || !strings.Contains(err.Error(), "unsupported ChatStorage.sqlite schema") {
		t.Fatalf("got %v", err)
	}
}

func TestUnknownMessageTypeIsExplicit(t *testing.T) {
	if got := renderPayload(999, "", "", "", 0, 0); got != "[WhatsApp message type 999]" {
		t.Fatalf("got %q", got)
	}
}

type scriptedFetcher struct {
	pages map[string]memory.Page
	errAt string
}

func (f scriptedFetcher) FetchPage(_ memory.ItemKind, _ memory.FetchWindow, cursor string) (memory.Page, error) {
	if cursor == f.errAt {
		return memory.Page{}, errors.New("injected page failure")
	}
	return f.pages[cursor], nil
}

func ingestItem(id string) memory.Item {
	conv := conversation{jid: id + "@s.whatsapp.net", title: id, messages: []message{{id: id, at: time.Now(), sender: id, body: "Please send the report tomorrow."}}}
	return memory.Item{Kind: KindConversation, ProviderID: conv.jid, Payload: conv}
}

func TestIngestStatusContract(t *testing.T) {
	t.Run("resume after page error", func(t *testing.T) {
		status := &memory.SyncStatus{}
		fetcher := scriptedFetcher{pages: map[string]memory.Page{"": {Items: []memory.Item{ingestItem("one")}, NextCursor: "next"}}, errAt: "next"}
		_, err := memory.Ingest(memory.IngestParams{Fetcher: fetcher, Kind: KindConversation, Status: status, Write: func(memory.MappedMemory) error { return nil }, Map: MapConversationFn()})
		if err == nil || status.Checkpoint != "next" || status.LastSuccessAt != "" {
			t.Fatalf("status=%+v err=%v", status, err)
		}
	})
	t.Run("partial write withholds success", func(t *testing.T) {
		status := &memory.SyncStatus{LastSuccessAt: "2026-01-01T00:00:00Z"}
		fetcher := scriptedFetcher{pages: map[string]memory.Page{"": {Items: []memory.Item{ingestItem("one"), ingestItem("two")}}}, errAt: "never"}
		calls := 0
		_, err := memory.Ingest(memory.IngestParams{Fetcher: fetcher, Kind: KindConversation, Status: status, Write: func(memory.MappedMemory) error {
			calls++
			if calls == 2 {
				return errors.New("disk full")
			}
			return nil
		}, Map: MapConversationFn()})
		if err == nil || status.LastSuccessAt != "2026-01-01T00:00:00Z" || status.ErrorCount != 1 {
			t.Fatalf("status=%+v err=%v", status, err)
		}
	})
	t.Run("clean run resets prior error", func(t *testing.T) {
		status := &memory.SyncStatus{ErrorCount: 2, LastError: "old"}
		fetcher := scriptedFetcher{pages: map[string]memory.Page{"": {Items: []memory.Item{ingestItem("one")}}}, errAt: "never"}
		_, err := memory.Ingest(memory.IngestParams{Fetcher: fetcher, Kind: KindConversation, Status: status, Write: func(memory.MappedMemory) error { return nil }, Map: MapConversationFn()})
		if err != nil || status.ErrorCount != 0 || status.LastError != "" || status.LastSuccessAt == "" {
			t.Fatalf("status=%+v err=%v", status, err)
		}
	})
}
