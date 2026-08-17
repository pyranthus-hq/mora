package commitment

import (
	"encoding/json"
	"github.com/pyranthus-hq/mora/internal/evidence"
	"testing"
)

func TestRecordJSONContract(t *testing.T) {
	citation, err := evidence.NewCitation("gmail_thread/t1", "gmail", "gmail:me", "2026-01-02T03:04:05Z")
	if err != nil {
		t.Fatal(err)
	}
	record := Record{ID: "commit_1", Owner: Atom{Kind: "address", Value: "me@example.com"}, Counterparty: Atom{Provider: "imessage", Kind: "handle", Value: "+1555"}, CounterpartyLabel: "Riya", CounterpartyKeys: []string{"riya"}, Direction: OwedBySelf, Summary: "send deck", OpenedBy: Span{MemoryID: "m1", MessageRef: "msg1", BlockRef: "b1", AncestorRefs: []string{"a1"}, Quote: "I will send", OccurredAt: "2026-01-02T03:04:05Z"}, Due: Due{Kind: DueExplicitDate, At: "2026-01-03"}, State: Open, ClosureRef: ClosureNone, SupersededBy: "commit_2", StateUncertain: true, Gap: "gap", Citations: []Citation{{Citation: citation, CommitmentID: "commit_1", Role: CitationOpener, EvidenceRef: "msg1"}}, DuplicateOf: "commit_0", ReviewedUseful: true}
	body, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":"commit_1","owner":{"kind":"address","value":"me@example.com"},"counterparty":{"provider":"imessage","kind":"handle","value":"+1555"},"counterparty_label":"Riya","counterparty_keys":["riya"],"direction":"owed_by_self","summary":"send deck","opened_by":{"memory_id":"m1","message_ref":"msg1","block_ref":"b1","ancestor_refs":["a1"],"quote":"I will send","occurred_at":"2026-01-02T03:04:05Z"},"due":{"kind":"explicit_date","at":"2026-01-03"},"state":"open","closure_ref":"none","superseded_by":"commit_2","state_uncertain":true,"gap":"gap","citations":[{"citation":{"memory_id":"gmail_thread/t1","channel":"gmail","source":"gmail:me","date":"2026-01-02T03:04:05Z"},"commitment_id":"commit_1","role":"opener","evidence_ref":"msg1"}],"duplicate_of":"commit_0","reviewed_useful":true}`
	if string(body) != want {
		t.Fatalf("json=%s\nwant=%s", body, want)
	}
	var round Record
	if err := json.Unmarshal(body, &round); err != nil {
		t.Fatal(err)
	}
	if round.OpenedBy.BlockRef != "b1" || round.Citations[0].Citation.MemoryID() != "gmail_thread/t1" || round.Owner.Value != "me@example.com" {
		t.Fatalf("round=%+v", round)
	}
}

func TestDeduplicateKeepsSupportingCitation(t *testing.T) {
	opened, _ := evidence.NewCitation("notes/original", "manual", "manual", "2026-07-20T10:00:00Z")
	copyCitation, _ := evidence.NewCitation("notes/copy", "manual", "manual", "2026-07-20T10:05:00Z")
	canonical := Record{ID: ID("notes/original#m1", "body", 0), Owner: Atom{Kind: "address", Value: "self@example.com"}, Counterparty: Atom{Kind: "address", Value: "sam@example.org"}, CounterpartyKeys: []string{"name:sam rivera", "given:sam"}, Direction: OwedBySelf, Summary: "Send Sam the reviewer list", OpenedBy: Span{MemoryID: "notes/original", MessageRef: "notes/original#m1", BlockRef: "body", Quote: "Can you send the reviewer list?", OccurredAt: "2026-07-20T10:00:00Z"}, Due: Due{Kind: DueNone}, State: Open, ClosureRef: ClosureNone, Citations: []Citation{{Citation: opened, Role: CitationOpener}}}
	copy := canonical
	copy.ID = ID("notes/copy#m1", "body", 0)
	copy.OpenedBy.MemoryID = "notes/copy"
	copy.OpenedBy.MessageRef = "notes/copy#m1"
	copy.OpenedBy.OccurredAt = "2026-07-20T10:05:00Z"
	copy.OpenedBy.AncestorRefs = []string{canonical.OpenedBy.MessageRef}
	copy.Counterparty = Atom{Provider: "imessage", Kind: "handle", Value: "+15550100123"}
	copy.Citations = []Citation{{Citation: copyCitation, CommitmentID: copy.ID, Role: CitationOpener}}
	got := Deduplicate([]Record{copy, canonical})
	if len(got) != 2 {
		t.Fatalf("len=%d", len(got))
	}
	byID := map[string]Record{}
	for _, record := range got {
		byID[record.ID] = record
	}
	if byID[copy.ID].DuplicateOf != canonical.ID {
		t.Fatalf("duplicate_of=%q", byID[copy.ID].DuplicateOf)
	}
	found := false
	for _, citation := range byID[canonical.ID].Citations {
		if citation.Role == CitationSupporting && citation.Citation.MemoryID() == "notes/copy" {
			found = true
		}
	}
	if !found {
		t.Fatalf("citations=%+v", byID[canonical.ID].Citations)
	}
	uncited := canonical
	uncited.ID = ""
	if got := Unique([]Record{uncited, uncited}); len(got) != 2 {
		t.Fatalf("unanchored records collapsed: %d", len(got))
	}
	if got := Unique([]Record{canonical, canonical}); len(got) != 1 {
		t.Fatalf("anchored duplicate not collapsed: %d", len(got))
	}
}
