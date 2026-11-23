package google

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
)

func TestGmailRunClassesAndAttempts(t *testing.T) {
	cases := []struct {
		name    string
		history string
		class   string
	}{
		{"empty", `{"historyId":"2"}`, "empty_delta"},
		{"labels", `{"historyId":"2","history":[{"messages":[{"threadId":"x"}],"labelsAdded":[{"message":{"threadId":"x"}}]}]}`, "label_churn"},
		{"message", `{"historyId":"2","history":[{"messagesAdded":[{"message":{"threadId":"x"}}],"labelsAdded":[{"message":{"threadId":"x"}}]}]}`, "new_threads"},
		{"deletion", `{"historyId":"2","history":[{"messagesDeleted":[{"message":{"threadId":"x"}}]}]}`, ""},
		{"label and deletion", `{"historyId":"2","history":[{"labelsAdded":[{"message":{"threadId":"x"}}],"messagesDeleted":[{"message":{"threadId":"x"}}]}]}`, ""},
		{"untyped", `{"historyId":"2","history":[{"messages":[{"threadId":"x"}]}]}`, ""},
		{"addition and deletion", `{"historyId":"2","history":[{"messagesAdded":[{"message":{"threadId":"x"}}],"messagesDeleted":[{"message":{"threadId":"x"}}]}]}`, "new_threads"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			attempts := 0
			svc, err := gmail.NewService(context.Background(), option.WithHTTPClient(&http.Client{Transport: ggRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				body := tc.history
				code := 200
				if strings.Contains(r.URL.Path, "/history") {
					attempts++
					if attempts == 1 {
						code = 503
						body = `{"error":{"code":503,"message":"retry"}}`
					}
				} else {
					body = `{"id":"x","messages":[]}`
				}
				return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})}))
			if err != nil {
				t.Fatal(err)
			}
			f := ggNewLiveFetcher(svc, nil)
			f.gmailWait = func(context.Context, time.Duration) error { return nil }
			if _, err = f.FetchPage(KindGmailThread, FetchWindow{SyncCursor: "1"}, ""); err != nil {
				t.Fatal(err)
			}
			got := f.GmailRunStats()
			if got.HistoryCalls != 2 || got.DeltaClass != tc.class {
				t.Fatalf("stats: %+v", got)
			}
		})
	}
}

func TestGmailRunIncompleteAndFallback(t *testing.T) {
	historyCalls := 0
	svc, err := gmail.NewService(context.Background(), option.WithHTTPClient(&http.Client{Transport: ggRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := `{"threads":[]}`
		code := 200
		if strings.Contains(r.URL.Path, "/history") {
			historyCalls++
			if historyCalls == 1 {
				body = `{"historyId":"2","nextPageToken":"next"}`
			} else {
				code = 404
				body = `{"error":{"code":404,"message":"expired"}}`
			}
		} else if strings.Contains(r.URL.Path, "/profile") {
			body = `{"historyId":"3"}`
		}
		return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}))
	if err != nil {
		t.Fatal(err)
	}
	f := ggNewLiveFetcher(svc, nil)
	f.gmailWait = func(context.Context, time.Duration) error { return nil }
	if _, err = f.FetchPage(KindGmailThread, FetchWindow{SyncCursor: "1"}, ""); err != nil {
		t.Fatal(err)
	}
	if got := f.GmailRunStats(); got.DeltaClass != "" {
		t.Fatalf("partial history classified: %+v", got)
	}
	if _, err = f.FetchPage(KindGmailThread, FetchWindow{SyncCursor: "1"}, "next"); err == nil {
		t.Fatal("expected expired cursor")
	}
	if _, err = f.FetchPage(KindGmailThread, FetchWindow{}, ""); err != nil {
		t.Fatal(err)
	}
	if got := f.GmailRunStats(); got.HistoryCalls != 2 || got.ThreadListCalls != 1 || got.DeltaClass != "expired_history" {
		t.Fatalf("fallback: %+v", got)
	}
	fresh := ggNewLiveFetcher(svc, nil)
	fresh.gmailWait = f.gmailWait
	if _, err = fresh.FetchPage(KindGmailThread, FetchWindow{}, ""); err != nil {
		t.Fatal(err)
	}
	if got := fresh.GmailRunStats(); got.DeltaClass != "initial_backfill" {
		t.Fatalf("backfill: %+v", got)
	}
}

func TestGmailRunListAndGetRetriesAcrossPages(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	svc, err := gmail.NewService(context.Background(), option.WithHTTPClient(&http.Client{Transport: ggRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		key := r.URL.Path + "?" + r.URL.Query().Get("pageToken")
		calls[key]++
		body := `{"id":"x","messages":[]}`
		code := 200
		switch {
		case strings.Contains(r.URL.Path, "/profile"):
			body = `{"historyId":"3"}`
		case strings.HasSuffix(r.URL.Path, "/threads"):
			if calls[key] == 1 {
				code = 503
				body = `{"error":{"code":503}}`
			} else if r.URL.Query().Get("pageToken") == "" {
				body = `{"threads":[{"id":"a"},{"id":"b"}],"nextPageToken":"next"}`
			} else {
				body = `{"threads":[{"id":"c"}]}`
			}
		case strings.Contains(r.URL.Path, "/threads/"):
			if calls[key] == 1 {
				code = 503
				body = `{"error":{"code":503}}`
			}
		}
		return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}))
	if err != nil {
		t.Fatal(err)
	}
	f := ggNewLiveFetcher(svc, nil)
	f.gmailWait = func(context.Context, time.Duration) error { return nil }
	page, err := f.FetchPage(KindGmailThread, FetchWindow{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.FetchPage(KindGmailThread, FetchWindow{}, page.NextCursor); err != nil {
		t.Fatal(err)
	}
	if got := f.GmailRunStats(); got.ThreadListCalls != 4 || got.ThreadGetCalls != 6 || got.HistoryCalls != 0 || got.DeltaClass != "initial_backfill" {
		t.Fatalf("stats: %+v", got)
	}
}
