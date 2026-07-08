package memory

import "testing"

// Issue #62 defect 2 (enrichment): Gmail actionability labels are captured into Meta
// but MUST be excluded from the content hash — otherwise reading (UNREAD dropped) or
// starring a thread would churn the hash and re-surface the thread as "[updated]" in
// the next brief. Labels are metadata, not content.
func TestMapItemExcludesLabelsFromContentHash(t *testing.T) {
	mk := func(labels []string) Item {
		meta := map[string]any{"from": []string{"a@b.com"}}
		if labels != nil {
			meta["labels"] = labels
		}
		return Item{Kind: "gmail_thread", ProviderID: "t1", Title: "Sub", Body: "Body", Meta: meta}
	}
	h0 := MapItem(mk(nil), "global", 0).ContentHash                             // pre-labels ingest
	h1 := MapItem(mk([]string{"UNREAD", "IMPORTANT"}), "global", 0).ContentHash // re-ingest with labels
	h2 := MapItem(mk([]string{"IMPORTANT"}), "global", 0).ContentHash           // read: UNREAD dropped
	if h0 != h1 || h1 != h2 {
		t.Fatalf("labels must not affect ContentHash: h0=%s h1=%s h2=%s", h0, h1, h2)
	}
	if m := MapItem(mk([]string{"STARRED"}), "global", 0); m.Meta["labels"] == nil {
		t.Fatalf("labels must still be PERSISTED in Meta (only excluded from the hash)")
	}
}
