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

func TestGmailHistorySkipsThreadsTheSnapshotQueryExcludes(t *testing.T) {
	threads := map[string]string{
		"promo":  `{"id":"promo","messages":[{"id":"m1","threadId":"promo","labelIds":["INBOX","CATEGORY_PROMOTIONS"],"payload":{"headers":[]}}]}`,
		"social": `{"id":"social","messages":[{"id":"m2","threadId":"social","labelIds":["CATEGORY_SOCIAL"],"payload":{"headers":[]}}]}`,
		"mixed":  `{"id":"mixed","messages":[{"id":"m3","threadId":"mixed","labelIds":["CATEGORY_PROMOTIONS"],"payload":{"headers":[]}},{"id":"m4","threadId":"mixed","labelIds":["INBOX"],"payload":{"headers":[]}}]}`,
		"normal": `{"id":"normal","messages":[{"id":"m5","threadId":"normal","labelIds":["INBOX","Label_work"],"payload":{"headers":[]}}]}`,
		"inbox":  `{"id":"inbox","messages":[{"id":"m7","threadId":"inbox","labelIds":["INBOX"],"payload":{"headers":[]}}]}`,
		"both":   `{"id":"both","messages":[{"id":"m8","threadId":"both","labelIds":["INBOX","IMPORTANT"],"payload":{"headers":[]}}]}`,
		"other":  `{"id":"other","messages":[{"id":"m6","threadId":"other","labelIds":["INBOX"],"payload":{"headers":[]}}]}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/history") {
			fmt.Fprint(w, `{"historyId":"200","history":[{"messages":[{"id":"m1","threadId":"promo"},{"id":"m2","threadId":"social"},{"id":"m3","threadId":"mixed"},{"id":"m5","threadId":"normal"},{"id":"m6","threadId":"other"},{"id":"m7","threadId":"inbox"},{"id":"m8","threadId":"both"}]}]}`)
			return
		}
		fmt.Fprint(w, threads[r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]])
	}))
	defer srv.Close()
	svc, err := gmail.NewService(context.Background(), option.WithEndpoint(srv.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	f := &LiveFetcher{gmail: svc}
	ids := func(labels []string) string {
		page, err := f.fetchGmailHistoryPage(context.Background(), FetchWindow{SyncCursor: "100", Labels: labels}, "")
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, it := range page.Items {
			got = append(got, it.ProviderID)
		}
		if page.SyncCursor != "200" {
			t.Fatalf("sync cursor = %q, want 200", page.SyncCursor)
		}
		return strings.Join(got, ",")
	}
	if got := ids(nil); got != "both,inbox,mixed,normal,other" {
		t.Fatalf("no labels: got %q, want both,inbox,mixed,normal,other", got)
	}
	if got := ids([]string{"Label_work"}); got != "normal" {
		t.Fatalf("with labels: got %q, want normal", got)
	}
	// threads.list LabelIds is AND: an INBOX-only thread must not match.
	if got := ids([]string{"INBOX", "IMPORTANT"}); got != "both" {
		t.Fatalf("two labels: got %q, want both", got)
	}
}
