package imessage

import (
	"context"
	"errors"
	"reflect"
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
