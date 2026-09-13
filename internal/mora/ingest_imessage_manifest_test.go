package mora

import (
	"context"
	"database/sql"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/imessage"
	"github.com/pyranthus-hq/mora/internal/memory"
)

type manifestCountingFetcher struct {
	*imessage.LiveFetcher
	rendered int
}

func (f *manifestCountingFetcher) FetchPageContext(ctx context.Context, kind memory.ItemKind, win memory.FetchWindow, cursor string) (memory.Page, error) {
	page, err := f.LiveFetcher.FetchPageContext(ctx, kind, win, cursor)
	f.rendered += len(page.Items)
	return page, err
}

func TestIMessageManifestRollingAndWidenedWindow(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	defer stubIMessageReadiness(t, true)()
	path := filepath.Join(t.TempDir(), "chat.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		`CREATE TABLE chat (ROWID INTEGER PRIMARY KEY, guid TEXT, display_name TEXT, chat_identifier TEXT)`,
		`CREATE TABLE handle (ROWID INTEGER PRIMARY KEY, id TEXT)`,
		`CREATE TABLE chat_handle_join (chat_id INTEGER, handle_id INTEGER)`,
		`CREATE TABLE message (ROWID INTEGER PRIMARY KEY, guid TEXT, date INTEGER, is_from_me INTEGER, text TEXT, attributedBody BLOB, associated_message_type INTEGER, item_type INTEGER, date_retracted INTEGER, handle_id INTEGER)`,
		`CREATE TABLE chat_message_join (chat_id INTEGER, message_id INTEGER)`,
		`CREATE TABLE attachment (ROWID INTEGER PRIMARY KEY, filename TEXT, mime_type TEXT, total_bytes INTEGER)`,
		`CREATE TABLE message_attachment_join (message_id INTEGER, attachment_id INTEGER)`,
		`INSERT INTO chat VALUES (1, 'unchanged-chat', '', 'friend')`,
		`INSERT INTO chat_message_join VALUES (1, 1)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO message (ROWID, guid, date, is_from_me, text) VALUES (1, 'message', ?, 1, 'hello')`, (time.Now().Add(-24*time.Hour).Unix()-978307200)*1_000_000_000); err != nil {
		t.Fatal(err)
	}
	original := newIMessageFetcher
	defer func() { newIMessageFetcher = original }()
	var fetched *manifestCountingFetcher
	newIMessageFetcher = func(string, imessage.DenyList) (iMessageFetcher, error) {
		live, err := imessage.NewLiveFetcher(path, imessage.DenyList{})
		if err != nil {
			return nil, err
		}
		fetched = &manifestCountingFetcher{LiveFetcher: live}
		return fetched, nil
	}
	source := Source{Type: "imessage", Name: "imessage", Scope: "personal", SinceDays: 30}
	ingest := func(want int) {
		t.Helper()
		if _, err := ingestIMessageDetailed(testCtx(t), cfg, source, io.Discard); err != nil {
			t.Fatal(err)
		}
		if fetched.rendered != want {
			t.Fatalf("rendered %d chats, want %d", fetched.rendered, want)
		}
	}
	ingest(1)
	manPath := imessageManifestPath(cfg, source.Name)
	man, err := imessage.LoadManifest(manPath)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a previous run one second earlier without sleeping or a clock hook.
	man.WindowStart -= int64(time.Second)
	if err := imessage.SaveManifest(manPath, man); err != nil {
		t.Fatal(err)
	}
	t.Run("rolling window skips unchanged chat", func(t *testing.T) {
		if _, err := ingestIMessageDetailed(testCtx(t), cfg, source, io.Discard); err != nil {
			t.Fatal(err)
		}
		if fetched.rendered != 0 {
			t.Fatalf("rolling run rendered %d chats, want 0", fetched.rendered)
		}
	})
	source.SinceDays = 60
	t.Run("widened window forces render", func(t *testing.T) {
		if _, err := ingestIMessageDetailed(testCtx(t), cfg, source, io.Discard); err != nil {
			t.Fatal(err)
		}
		if fetched.rendered != 1 {
			t.Fatalf("widened run rendered %d chats, want 1", fetched.rendered)
		}
	})
}
