package google

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gmail "google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
)

func concurrentGmailFetcher(t *testing.T, handler http.HandlerFunc) *LiveFetcher {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	svc, err := gmail.NewService(context.Background(), option.WithEndpoint(srv.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	return &LiveFetcher{gmail: svc}
}

func TestGmailThreadsOverlapBoundedAndKeepOrder(t *testing.T) {
	entered := make(chan string, 16)
	release := make(chan struct{})
	var active, peak atomic.Int32
	f := concurrentGmailFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		entered <- id
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":%q}`, id)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ids := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	type result struct {
		threads []*gmail.Thread
		err     error
	}
	done := make(chan result, 1)
	go func() {
		threads, err := f.getGmailThreads(ctx, ids)
		done <- result{threads, err}
	}()
	// Every worker must reach the server before any response is released. A
	// sequential implementation cannot pass this barrier.
	for range gmailThreadConcurrency {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("thread requests did not overlap")
		}
	}
	close(release)
	got := <-done
	if got.err != nil {
		t.Fatal(got.err)
	}
	if peak.Load() > gmailThreadConcurrency {
		t.Fatalf("unbounded requests: peak=%d", peak.Load())
	}
	for i, th := range got.threads {
		if th == nil || th.Id != ids[i] {
			t.Fatalf("slot %d: got %+v, want %s", i, th, ids[i])
		}
	}
}

func TestGmailThreadsSharedPacingAndRetryGate(t *testing.T) {
	var calls atomic.Int32
	f := concurrentGmailFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"code":429,"message":"retry"}}`)
			return
		}
		fmt.Fprint(w, `{"id":"ok"}`)
	})
	var pacingActive, paceCalls atomic.Int32
	var concurrentPacing atomic.Bool
	f.gmailWait = func(ctx context.Context, delay time.Duration) error {
		if delay != 500*time.Millisecond {
			return nil // retry backoff is independent of request admission
		}
		paceCalls.Add(1)
		if pacingActive.Add(1) != 1 {
			concurrentPacing.Store(true)
		}
		defer pacingActive.Add(-1)
		return waitGoogle(ctx, time.Millisecond)
	}
	_, err := f.getGmailThreads(context.Background(), []string{"a", "b", "c", "d", "e"})
	if err != nil {
		t.Fatal(err)
	}
	if concurrentPacing.Load() {
		t.Fatal("workers multiplied the pacing budget")
	}
	if paceCalls.Load() != calls.Load() || calls.Load() != 6 {
		t.Fatalf("every attempt must use pacing, got waits=%d requests=%d", paceCalls.Load(), calls.Load())
	}
}

func TestGmailThreadsFailureCancelsAndJoins(t *testing.T) {
	entered := make(chan struct{}, gmailThreadConcurrency)
	var once sync.Once
	allStarted := make(chan struct{})
	var starts atomic.Int32
	f := concurrentGmailFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		if starts.Add(1) == gmailThreadConcurrency {
			once.Do(func() { close(allStarted) })
		}
		select {
		case <-allStarted:
		case <-r.Context().Done():
			return
		}
		if strings.HasSuffix(r.URL.Path, "/bad") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"error":{"code":403,"message":"forbidden"}}`)
			return
		}
		<-r.Context().Done()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	threads, err := f.getGmailThreads(ctx, []string{"bad", "b", "c", "d", "e", "f"})
	if err == nil || threads != nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		t.Fatalf("lost initiating error or exposed partial page: threads=%v err=%v", threads, err)
	}
	if starts.Load() != gmailThreadConcurrency {
		t.Fatalf("scheduled extra work after failure: %d requests", starts.Load())
	}
}

func TestGmailThreadsAlreadyCancelledDoesNotRequest(t *testing.T) {
	f := concurrentGmailFetcher(t, func(http.ResponseWriter, *http.Request) {
		t.Error("request after cancellation")
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.getGmailThreads(ctx, []string{"a"}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestGmailConcurrentPagePreserves404Semantics(t *testing.T) {
	f := concurrentGmailFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/profile"):
			fmt.Fprint(w, `{"historyId":"100"}`)
		case strings.HasSuffix(r.URL.Path, "/threads"):
			fmt.Fprint(w, `{"threads":[{"id":"c"},{"id":"b"},{"id":"a"}],"nextPageToken":"next"}`)
		case strings.HasSuffix(r.URL.Path, "/history"):
			fmt.Fprint(w, `{"historyId":"200","history":[{"messages":[{"threadId":"c"},{"threadId":"b"},{"threadId":"a"}]}]}`)
		case strings.HasSuffix(r.URL.Path, "/b"):
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":{"code":404,"message":"deleted"}}`)
		default:
			id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			fmt.Fprintf(w, `{"id":%q}`, id)
		}
	})
	snapshot, err := f.fetchGmailPageContext(context.Background(), FetchWindow{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Items) != 2 || snapshot.Items[0].ProviderID != "c" || snapshot.Items[1].ProviderID != "a" || snapshot.NextCursor != "next" || snapshot.SyncCursor != "100" {
		t.Fatalf("snapshot did not preserve list order/skip 404: %+v", snapshot)
	}
	history, err := f.fetchGmailHistoryPage(context.Background(), FetchWindow{SyncCursor: "100"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Items) != 3 || history.Items[0].ProviderID != "a" || history.Items[1].ProviderID != "b" || !history.Items[1].Deleted || history.Items[2].ProviderID != "c" || history.SyncCursor != "200" {
		t.Fatalf("history did not preserve sorted order/tombstone: %+v", history)
	}
}

// BenchmarkGmailThreadLatency isolates HTTP wait overlap with a local server.
// Production retains its shared 500ms pacing gate; this fixture removes pacing
// so the benchmark measures network-wait serialization, not provider quotas.
func BenchmarkGmailThreadLatency(b *testing.B) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := waitGoogle(r.Context(), 10*time.Millisecond); err != nil {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"fixture"}`)
	}))
	defer srv.Close()
	svc, err := gmail.NewService(context.Background(), option.WithEndpoint(srv.URL), option.WithoutAuthentication())
	if err != nil {
		b.Fatal(err)
	}
	f := &LiveFetcher{gmail: svc}
	ids := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	b.Run("serial", func(b *testing.B) {
		for b.Loop() {
			for _, id := range ids {
				if _, err := f.getGmailThread(context.Background(), id); err != nil {
					b.Fatal(err)
				}
			}
		}
	})
	b.Run("bounded_parallel", func(b *testing.B) {
		for b.Loop() {
			if _, err := f.getGmailThreads(context.Background(), ids); err != nil {
				b.Fatal(err)
			}
		}
	})
}
