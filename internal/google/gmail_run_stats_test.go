package google

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
)

// AC-001.1 An empty incremental Gmail run reports one history call, zero list and get calls, and empty_delta.
func TestGmailRunStatsReceiptEmptyDelta(t *testing.T) {
	fake := &ggFakeGoogle{history: func(*http.Request) (int, string) {
		return 200, `{"historyId":"9001","history":[]}`
	}}
	fetcher := ggNewLiveFetcher(ggGmailSvc(t, ggServe(t, fake)), nil)
	if _, err := fetcher.FetchPage(KindGmailThread, FetchWindow{SyncCursor: "9000"}, ""); err != nil {
		t.Fatal(err)
	}
	stats := reflect.ValueOf(fetcher).MethodByName("GmailRunStats")
	if !stats.IsValid() {
		t.Fatal("missing per-run Gmail request counts and delta class")
	}
	raw, err := json.Marshal(stats.Call(nil)[0].Interface())
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"history_calls": float64(1), "thread_list_calls": float64(0), "thread_get_calls": float64(0), "delta_class": "empty_delta"}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("%s = %v, want %v", key, got[key], value)
		}
	}
}
