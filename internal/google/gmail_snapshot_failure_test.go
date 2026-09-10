package google

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gmail "google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
)

func TestGmailSnapshotDoesNotSwallowThreadFailures(t *testing.T) {
	for _, status := range []int{403, 429, 500, 404} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/profile"):
					fmt.Fprint(w, `{"historyId":"123"}`)
				case strings.HasSuffix(r.URL.Path, "/threads"):
					fmt.Fprint(w, `{"threads":[{"id":"missing"}],"nextPageToken":"next"}`)
				default:
					w.WriteHeader(status)
					fmt.Fprintf(w, `{"error":{"code":%d,"message":"fixture failure"}}`, status)
				}
			}))
			defer srv.Close()
			svc, err := gmail.NewService(context.Background(), option.WithEndpoint(srv.URL), option.WithoutAuthentication())
			if err != nil {
				t.Fatal(err)
			}
			f := &LiveFetcher{gmail: svc}
			page, err := f.fetchGmailPageContext(context.Background(), FetchWindow{}, "")
			if status == 404 {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || page.NextCursor != "" || page.SyncCursor != "" {
				t.Fatalf("failed thread advanced snapshot: %+v %v", page, err)
			}
		})
	}
}
