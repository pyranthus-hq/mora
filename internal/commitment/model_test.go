package commitment

import (
	"encoding/json"
	"github.com/pyranthus-hq/mora/internal/evidence"
	"github.com/pyranthus-hq/mora/internal/memory"
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

func TestAcceptanceAndReportedActorPolicies(t *testing.T) {
	self := Atom{Kind: "address", Value: "me@example.com"}
	other := Atom{Kind: "address", Value: "sam@example.com"}
	opener := Record{Owner: self, Counterparty: other, Direction: OwedBySelf, Summary: "Can you send the reviewer list?", OpenedBy: Span{MemoryID: "thread", MessageRef: "m1", OccurredAt: "2026-01-01T10:00:00Z"}, Due: Due{Kind: DueNone}, State: Open, ClosureRef: ClosureNone}
	accepted := opener
	accepted.Summary = "I'll send the reviewer list"
	accepted.OpenedBy.MessageRef = "m2"
	accepted.OpenedBy.OccurredAt = "2026-01-01T10:01:00Z"
	if i, ok := AcceptanceRestatesRequest([]Record{opener}, accepted); !ok || i != 0 {
		t.Fatalf("acceptance=%d,%v", i, ok)
	}
	same := accepted
	same.OpenedBy.MessageRef = "m1"
	if _, ok := AcceptanceRestatesRequest([]Record{opener}, same); ok {
		t.Fatal("same-message acceptance merged")
	}
	unrelated := accepted
	unrelated.Summary = "I'll book the venue"
	if _, ok := AcceptanceRestatesRequest([]Record{opener}, unrelated); ok {
		t.Fatal("unrelated acceptance merged")
	}
	sam := NamedActor{Atom: other, Name: "Sam Rivera"}
	if actor, attributed := ReportedActor("Sam said: I'll send it", other, self, []NamedActor{sam}, nil); !attributed || actor == nil || !EqualAtom(*actor, other) {
		t.Fatalf("counterparty actor=%+v,%v", actor, attributed)
	}
	alex := NamedActor{Atom: Atom{Kind: "address", Value: "alex@example.com"}, Name: "Alex Chen"}
	if actor, attributed := ReportedActor("Alex will send it to Adit", other, self, []NamedActor{alex}, []string{"Adit Karode"}); !attributed || actor == nil || actor.Value != "alex@example.com" {
		t.Fatalf("beneficiary actor=%+v,%v", actor, attributed)
	}
	if actor, attributed := ReportedActor("Alex will send it", other, self, []NamedActor{alex}, []string{"Adit Karode"}); !attributed || actor != nil {
		t.Fatalf("third-party actor=%+v,%v", actor, attributed)
	}
	if actor, attributed := ReportedActor("No attribution", other, self, []NamedActor{sam}, nil); attributed || actor != nil {
		t.Fatalf("unattributed=%+v,%v", actor, attributed)
	}
	if actor, attributed := ReportedActor("Sam said Alex will send", other, self, []NamedActor{sam, alex}, nil); !attributed || actor != nil {
		t.Fatalf("ambiguous=%+v,%v", actor, attributed)
	}
}

func TestNewRecordAndOpenerCitation(t *testing.T) {
	m := memory.Memory{ID: "gmail_thread/t1", Provider: "gmail", Source: "gmail:me", CreatedAt: "2026-01-01T10:00:00Z", Meta: map[string]any{"from": []string{"sam@example.com"}}}
	ancestors := []string{"m0"}
	record := NewRecord(m, "  I will send   tomorrow. ", "gmail_thread/t1#m1", "body", "2026-01-01T10:00:00Z", ancestors, 2, Atom{Kind: AtomAddress, Value: "me@example.com"}, Atom{Kind: AtomAddress, Value: "sam@example.com"}, OwedBySelf)
	ancestors[0] = "mutated"
	if record.ID == "" || record.Summary != "I will send tomorrow." || record.OpenedBy.Quote != record.Summary || record.OpenedBy.AncestorRefs[0] != "m0" || record.State != Open || record.ClosureRef != ClosureNone || record.Due.Kind != DueRelative || len(record.Citations) != 1 || record.Citations[0].EvidenceRef != "" || record.Citations[0].Citation.Date() != "2026-01-01T10:00:00Z" {
		t.Fatalf("record=%+v", record)
	}
	im := memory.Memory{ID: "imessage_chat/t1", Provider: "imessage", Source: "chat", CreatedAt: "2026-01-01T09:00:00Z"}
	citations := OpenerCitations(im, "c1", "imessage_chat/t1#m1", "2026-01-01T11:00:00Z")
	if len(citations) != 1 || citations[0].EvidenceRef != "imessage_chat/t1#m1" || citations[0].Citation.Date() != "2026-01-01T11:00:00Z" {
		t.Fatalf("im citations=%+v", citations)
	}
	bad := m
	bad.ID = ""
	if got := OpenerCitations(bad, "c", "m", "2026-01-01T10:00:00Z"); len(got) != 0 {
		t.Fatalf("bad=%+v", got)
	}
}
