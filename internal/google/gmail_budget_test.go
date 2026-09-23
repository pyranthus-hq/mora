package google

import (
	"net/http"
	"strings"
	"testing"
)

func TestGmailEmptyDeltaHasConstantRequestBudget(t *testing.T) {
	// The fake mailbox size is deliberately irrelevant: a stored history cursor
	// asks for changes and makes no threads.list or threads.get calls.
	g := &ggFakeGoogle{history: func(*http.Request) (int, string) {
		return 200, `{"historyId":"9001","history":[]}`
	}}
	f := ggNewLiveFetcher(ggGmailSvc(t, ggServe(t, g)), nil)
	page, err := f.FetchPage(KindGmailThread, FetchWindow{SyncCursor: "9000"}, "")
	if err != nil {
		t.Fatal(err)
	}
	requests := g.recorded()
	if len(requests) != 1 || !strings.Contains(requests[0], "/history") {
		t.Fatalf("empty delta must use exactly one history call, got %v", requests)
	}
	if len(page.Items) != 0 || page.NextCursor != "" || page.SyncCursor != "9001" {
		t.Fatalf("wrong empty-delta result: %+v", page)
	}
}
