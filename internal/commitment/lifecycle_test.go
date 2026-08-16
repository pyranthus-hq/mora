package commitment

import "testing"

func lifecycleItem() Item {
	return Item{ID: "c1", Owner: Atom{Kind: "address", Value: "self@example.com"}, Counterparty: Atom{Kind: "address", Value: "sam@example.com"}, CounterpartyKeys: []string{"name:sam rivera"}, Direction: OwedBySelf, Summary: "Send the reviewer list", OpenedBy: Span{MemoryID: "gmail_thread/open", MessageRef: "gmail_thread/open#m1", BlockRef: "body", Quote: "I will send the reviewer list", OccurredAt: "2026-07-20T10:00:00Z"}, Due: Due{Kind: DueNone}, State: Open, ClosureRef: "none"}
}
func TestCommitmentLifecycleGuards(t *testing.T) {
	tests := []struct {
		name, text string
		party      Party
		keys       []string
		authored   bool
		want       string
	}{{"negation", "I haven't sent the reviewer list.", PartySelf, nil, true, Open}, {"modality", "I will send the reviewer list.", PartySelf, nil, true, Open}, {"question", "Did you send the reviewer list?", PartyCounterparty, nil, true, Open}, {"question no punctuation", "Did you send the reviewer list", PartyCounterparty, nil, true, Open}, {"staged", "The reviewer list is staged.", PartySelf, nil, true, Open}, {"quoted", "I sent the reviewer list.", PartySelf, nil, false, Open}, {"wrong counterparty", "I sent the reviewer list.", PartySelf, []string{"name:bob jones"}, true, Open}, {"delivered", "I sent the reviewer list.", PartySelf, nil, true, Closed}, {"done", "Done with the reviewer list.", PartySelf, nil, true, Closed}, {"ack", "Got the reviewer list, thanks.", PartyCounterparty, nil, true, Closed}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keys := tt.keys
			if len(keys) == 0 {
				keys = []string{"name:sam rivera"}
			}
			got := ProjectLifecycle([]Item{lifecycleItem()}, []Evidence{{MemoryID: "imessage_chat/closure", Text: tt.text, OccurredAt: "2026-07-20T11:00:00Z", Party: tt.party, Authored: tt.authored, CounterpartyKeys: keys}})
			if got[0].Item.State != tt.want {
				t.Fatalf("state=%q want=%q", got[0].Item.State, tt.want)
			}
		})
	}
}
func TestCommitmentSupersededIsNotClosed(t *testing.T) {
	got := ProjectLifecycle([]Item{lifecycleItem()}, []Evidence{{MemoryID: "gmail_thread/replacement", Text: "The reviewer list deadline moved to Monday instead.", OccurredAt: "2026-07-20T11:00:00Z", Party: PartyCounterparty, Authored: true, CounterpartyKeys: []string{"name:sam rivera"}}})[0]
	if got.Item.State != Superseded || got.Item.SupersededBy != "gmail_thread/replacement" || got.Item.ClosureRef != "none" || got.ClosureEvidence != -1 {
		t.Fatalf("got=%+v", got)
	}
}
func TestCommitmentClosureRefusesMultipleCandidates(t *testing.T) {
	a, b := lifecycleItem(), lifecycleItem()
	b.ID = "c2"
	b.OpenedBy.MemoryID = "gmail_thread/second"
	b.OpenedBy.MessageRef = "gmail_thread/second#m1"
	got := ProjectLifecycle([]Item{a, b}, []Evidence{{MemoryID: "imessage_chat/closure", Text: "I sent the reviewer list.", OccurredAt: "2026-07-20T11:00:00Z", Party: PartySelf, Authored: true, CounterpartyKeys: []string{"name:sam rivera"}}})
	for _, v := range got {
		if v.Item.State != Open || v.Item.Gap == "" {
			t.Fatalf("got=%+v", got)
		}
	}
}
func TestDistinctObligationsWithSameTextAreNotMerged(t *testing.T) {
	a, b := lifecycleItem(), lifecycleItem()
	b.ID = "c2"
	b.OpenedBy.MemoryID = "gmail_thread/second"
	b.OpenedBy.MessageRef = "gmail_thread/second#m1"
	got := ProjectDuplicates([]Item{a, b})
	if len(got) != 2 || got[0].Item.DuplicateOf != "" || got[1].Item.DuplicateOf != "" {
		t.Fatalf("got=%+v", got)
	}
}
func TestCommitmentDedupRequiresStrongProvenance(t *testing.T) {
	canonical, copy := lifecycleItem(), lifecycleItem()
	copy.ID = "copy"
	copy.OpenedBy.MemoryID = "notes/copy"
	copy.OpenedBy.MessageRef = "notes/copy#m1"
	copy.OpenedBy.AncestorRefs = []string{canonical.OpenedBy.MessageRef}
	copy.Counterparty = Atom{Kind: "address", Value: "different@example.com"}
	copy.CounterpartyKeys = []string{"name:sam rivera", "given:sam"}
	got := ProjectDuplicates([]Item{copy, canonical})
	byID := map[string]DedupResult{}
	for _, v := range got {
		byID[v.Item.ID] = v
	}
	if byID["copy"].Item.DuplicateOf != "c1" || len(byID["c1"].SupportingOriginalIndexes) != 1 {
		t.Fatalf("got=%+v", got)
	}
}
func TestLifecycleEvidenceOrderingReturnsOriginalIndex(t *testing.T) {
	early := Evidence{MemoryID: "z", Text: "I sent the reviewer list.", OccurredAt: "2026-07-20T11:00:00Z", Party: PartySelf, Authored: true, CounterpartyKeys: []string{"name:sam rivera"}}
	late := early
	late.MemoryID = "a"
	late.OccurredAt = "2026-07-20T12:00:00Z"
	got := ProjectLifecycle([]Item{lifecycleItem()}, []Evidence{late, early})[0]
	if got.ClosureEvidence != 1 || got.Item.ClosureRef != "z" {
		t.Fatalf("got=%+v", got)
	}
}

func TestLifecycleCompatibilityHelpers(t *testing.T) {
	for _, text := range []string{"deadline moved to monday", "this is cancelled", "got the reviewer list", "we uploaded the reviewer list", "nothing changed"} {
		Transition(text)
	}
	if !StrictlyAfter("2026-01-01T00:00:00Z", "2026-01-02T00:00:00Z") || StrictlyAfter("bad", "2026-01-02T00:00:00Z") {
		t.Fatal("strict ordering")
	}
	if ObjectOverlap("reviewer lists", "reviewer list") != 2 {
		t.Fatal("overlap")
	}
	if !ContainsStringFold([]string{" A ", "b"}, "a") || ContainsStringFold(nil, "a") || ContainsStringFold([]string{"a"}, "") {
		t.Fatal("contains")
	}
	a := lifecycleItem()
	b := a
	b.ID = "b"
	if !DedupCandidate(a, b) {
		t.Fatal("exact candidate")
	}
	b.Summary = "reviewer list due"
	if !DedupCandidate(a, b) {
		t.Fatal("overlap candidate")
	}
	b.Summary = "x"
	if DedupCandidate(a, b) {
		t.Fatal("empty candidate")
	}
	b.Summary = "unrelated zebra"
	if DedupCandidate(a, b) {
		t.Fatal("weak candidate")
	}
	cases := [][2]Item{{a, b}}
	_ = cases
	b = a
	b.OpenedBy.OccurredAt = "2026-07-21T00:00:00Z"
	if !EvidenceLess(a, b) {
		t.Fatal("time")
	}
	b = a
	b.OpenedBy.OccurredAt = "bad2"
	a.OpenedBy.OccurredAt = "bad1"
	if !EvidenceLess(a, b) {
		t.Fatal("lexical time")
	}
	a, b = lifecycleItem(), lifecycleItem()
	a.OpenedBy.MessageRef = "a"
	b.OpenedBy.MessageRef = "b"
	if !EvidenceLess(a, b) {
		t.Fatal("message")
	}
	a, b = lifecycleItem(), lifecycleItem()
	a.OpenedBy.BlockRef = "a"
	b.OpenedBy.BlockRef = "b"
	if !EvidenceLess(a, b) {
		t.Fatal("block")
	}
	a, b = lifecycleItem(), lifecycleItem()
	a.ID = "a"
	b.ID = "b"
	if !EvidenceLess(a, b) {
		t.Fatal("id")
	}
}
func TestLifecycleRejectsWeakClosureEvidence(t *testing.T) {
	base := lifecycleItem()
	cases := []Evidence{{Text: "I sent the reviewer list.", OccurredAt: "2026-07-20T11:00:00Z", Party: PartyUnknown, Authored: true, CounterpartyKeys: []string{"name:sam rivera"}}, {MemoryID: "other", Text: "I sent the reviewer list.", OccurredAt: "2026-07-20T09:00:00Z", Party: PartySelf, Authored: true, CounterpartyKeys: []string{"name:sam rivera"}}, {MemoryID: "other", Text: "I sent the reviewer list.", OccurredAt: "2026-07-20T11:00:00Z", Party: PartySelf, Authored: true, CounterpartyKeys: []string{"name:bob"}}, {MemoryID: "other", Text: "Got the reviewer list.", OccurredAt: "2026-07-20T11:00:00Z", Party: PartySelf, Authored: true, CounterpartyKeys: []string{"name:sam rivera"}}, {MemoryID: "other", Text: "I sent the unrelated zebra.", OccurredAt: "2026-07-20T11:00:00Z", Party: PartySelf, Authored: true, CounterpartyKeys: []string{"name:sam rivera"}}}
	for i, e := range cases {
		got := ProjectLifecycle([]Item{base}, []Evidence{e})[0]
		if got.Item.State != Open {
			t.Fatalf("case %d closed: %+v", i, got)
		}
	}
}
