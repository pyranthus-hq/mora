package applecal

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/pyranthus-hq/mora/internal/memory"
)

func TestFetchPageContextCancelledBeforeQuery(t *testing.T) {
	f, err := NewLiveFetcher(seedDB(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = f.FetchPageContext(ctx, KindAppleCalEvent, memory.FetchWindow{}, "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("FetchPageContext error = %v, want context.Canceled", err)
	}
}

func TestFetchPageContextCancellationStopsParticipantLookup(t *testing.T) {
	f, err := NewLiveFetcher(seedDB(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	testHookBeforeParticipantLookup = cancel
	t.Cleanup(func() { testHookBeforeParticipantLookup = nil })

	_, err = f.FetchPageContext(ctx, KindAppleCalEvent, memory.FetchWindow{}, "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("FetchPageContext error = %v, want context.Canceled", err)
	}
}

func TestFetchPageCompatibilityWrapper(t *testing.T) {
	f, err := NewLiveFetcher(seedDB(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	w := memory.FetchWindow{}
	want, err := f.FetchPageContext(context.Background(), KindAppleCalEvent, w, "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.FetchPage(KindAppleCalEvent, w, "")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("FetchPage = %#v, want %#v", got, want)
	}
}
