package imessage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func contextTestFetcher(t *testing.T) *LiveFetcher {
	t.Helper()
	path := seedChatDB(t,
		[]seedChat{{rowid: 1, guid: "chat-1", identifier: "+14155550100", participants: []string{"+14155550100"}}},
		[]seedMsg{{chatID: 1, text: "hello", handle: "+14155550100"}},
	)
	f, err := NewLiveFetcher(path, DenyList{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func TestFetchPageContextCancelledBeforeQuery(t *testing.T) {
	f := contextTestFetcher(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := f.FetchPageContext(ctx, KindIMessageChat, FetchWindow{}, "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("FetchPageContext error = %v, want context.Canceled", err)
	}
}

func TestFetchPageContextCancellationStopsPaging(t *testing.T) {
	f := contextTestFetcher(t)
	ctx, cancel := context.WithCancel(context.Background())
	testHookAfterChatList = cancel
	t.Cleanup(func() { testHookAfterChatList = nil })

	_, err := f.FetchPageContext(ctx, KindIMessageChat, FetchWindow{}, "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("FetchPageContext error = %v, want context.Canceled", err)
	}
}

func TestFetchPageCompatibilityWrapper(t *testing.T) {
	f := contextTestFetcher(t)
	want, err := f.FetchPageContext(context.Background(), KindIMessageChat, FetchWindow{}, "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.FetchPage(KindIMessageChat, FetchWindow{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("FetchPage = %#v, want %#v", got, want)
	}
}

func threeChatFetcher(t *testing.T) (*LiveFetcher, string) {
	t.Helper()
	path := seedChatDB(t,
		[]seedChat{
			{rowid: 1, guid: "chat-1", identifier: "+14155550100", participants: []string{"+14155550100"}},
			{rowid: 42, guid: "chat-42", identifier: "+14155550142", participants: []string{"+14155550142"}},
			{rowid: 43, guid: "chat-43", identifier: "+14155550143", participants: []string{"+14155550143"}},
		},
		[]seedMsg{
			{chatID: 1, text: "hello", handle: "+14155550100"},
			{chatID: 42, text: "world", handle: "+14155550142"},
			{chatID: 43, text: "again", handle: "+14155550143"},
		},
	)
	f, err := NewLiveFetcher(path, DenyList{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f, path
}

func injectChatErr(t *testing.T, rowid int64, err error) {
	t.Helper()
	testHookChatReadErr = func(r int64) error {
		if r == rowid {
			return err
		}
		return nil
	}
	t.Cleanup(func() { testHookChatReadErr = nil })
}

// An error scoped to one chat must not discard the healthy chats: the bad chat
// arrives as an identity-bearing item with FetchError so Ingest can count it.
func TestFetchPageContextIsolatedChatErrorKeepsPage(t *testing.T) {
	f, _ := threeChatFetcher(t)
	injectChatErr(t, 42, errors.New("chat 42 unreadable"))

	page, err := f.FetchPageContext(context.Background(), KindIMessageChat, FetchWindow{}, "")
	if err != nil {
		t.Fatalf("FetchPageContext error = %v, want nil", err)
	}
	var ids []string
	for _, it := range page.Items {
		ids = append(ids, it.ProviderID)
		if (it.ProviderID == "chat-42") != (it.FetchError != nil) {
			t.Fatalf("item %s FetchError = %v", it.ProviderID, it.FetchError)
		}
	}
	if !reflect.DeepEqual(ids, []string{"chat-1", "chat-42", "chat-43"}) {
		t.Fatalf("items = %v, want all three chats", ids)
	}
}

// Context errors fail the page (checkpoint kept), even when the context itself is
// still live, e.g. a driver-side deadline.
func TestFetchPageContextDeadlineErrorFailsPage(t *testing.T) {
	f, _ := threeChatFetcher(t)
	injectChatErr(t, 42, fmt.Errorf("conversation query: %w", context.DeadlineExceeded))
	if _, err := f.FetchPageContext(context.Background(), KindIMessageChat, FetchWindow{}, ""); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want DeadlineExceeded", err)
	}
}

// Messages.app holding the write lock (SQLITE_BUSY) hits every chat, so the page
// fails and the next run retries it rather than recording each chat as bad.
func TestFetchPageContextBusyFailsPage(t *testing.T) {
	f, path := threeChatFetcher(t)
	f.db.SetMaxOpenConns(1)
	if _, err := f.db.Exec(`PRAGMA busy_timeout=50`); err != nil {
		t.Fatal(err)
	}
	locker, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = locker.Close() })
	conn, err := locker.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = conn.ExecContext(context.Background(), `ROLLBACK`); _ = conn.Close() })
	testHookAfterChatList = func() {
		if _, err := conn.ExecContext(context.Background(), `BEGIN EXCLUSIVE`); err != nil {
			t.Fatalf("take lock: %v", err)
		}
	}
	t.Cleanup(func() { testHookAfterChatList = nil })

	page, err := f.FetchPageContext(context.Background(), KindIMessageChat, FetchWindow{}, "")
	if err == nil {
		t.Fatalf("error = nil, want a lock error; page has %d items", len(page.Items))
	}
	if !strings.Contains(err.Error(), "SQLITE_BUSY") {
		t.Fatalf("error = %v, want SQLITE_BUSY", err)
	}
}

// A broken source query (a missing table) is not one chat's problem: it stays fatal.
func TestFetchPageContextBrokenQueryFailsPage(t *testing.T) {
	f, path := threeChatFetcher(t)
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE attachment`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if _, err := f.FetchPageContext(context.Background(), KindIMessageChat, FetchWindow{}, ""); err == nil {
		t.Fatal("error = nil, want a conversation query error")
	}
}
