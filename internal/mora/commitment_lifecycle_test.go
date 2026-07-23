package mora

import (
	"reflect"
	"testing"
	"time"
)

func lifecycleTestCommitment() Commitment {
	self := govAtom{Kind: atomAddress, Value: "self@example.com"}
	other := govAtom{Kind: atomAddress, Value: "sam@example.org"}
	return Commitment{
		ID:               commitmentID("gmail_thread/open#m1", "body", 0),
		Owner:            self,
		Counterparty:     other,
		CounterpartyKeys: []string{"name:sam rivera", "given:sam"},
		Direction:        commitOwedBySelf,
		Summary:          "Send Sam the reviewer list",
		OpenedBy: commitSpan{
			MemoryID:   "gmail_thread/open",
			MessageRef: "gmail_thread/open#m1",
			BlockRef:   "body",
			Quote:      "Can you send the reviewer list?",
			OccurredAt: "2026-07-20T10:00:00Z",
		},
		Due:        commitDue{Kind: commitDueNone},
		State:      commitOpen,
		ClosureRef: commitClosureNone,
		Citations: []CommitmentCitation{{
			Citation:     mustLifecycleCitation("gmail_thread/open", "2026-07-20T10:00:00Z"),
			CommitmentID: commitmentID("gmail_thread/open#m1", "body", 0),
			Role:         commitCitationOpener,
		}},
	}
}

func mustLifecycleCitation(memoryID, at string) BriefCitation {
	citation, err := newBriefCitation(memoryID, "gmail", memoryID, at)
	if err != nil {
		panic(err)
	}
	return citation
}

func TestCommitmentLifecycleGuards(t *testing.T) {
	tests := []struct {
		name string
		text string
		role commitmentPartyRole
		keys []string
		want string
	}{
		{name: "negation", text: "I haven't sent the reviewer list.", role: commitmentPartySelf, want: commitOpen},
		{name: "modality", text: "I will send the reviewer list.", role: commitmentPartySelf, want: commitOpen},
		{name: "question", text: "Did you send the reviewer list?", role: commitmentPartyCounterparty, want: commitOpen},
		{name: "question without punctuation", text: "Did you send the reviewer list", role: commitmentPartyCounterparty, want: commitOpen},
		{name: "staged", text: "The reviewer list is staged.", role: commitmentPartySelf, want: commitOpen},
		{name: "quoted", text: "I sent the reviewer list.", role: commitmentPartySelf, want: commitOpen},
		{name: "wrong counterparty", text: "I sent the reviewer list.", role: commitmentPartySelf, keys: []string{"name:bob jones"}, want: commitOpen},
		{name: "delivered", text: "I sent the reviewer list.", role: commitmentPartySelf, want: commitClosed},
		{name: "done", text: "Done with the reviewer list.", role: commitmentPartySelf, want: commitClosed},
		{name: "acknowledged", text: "Got the reviewer list, thanks.", role: commitmentPartyCounterparty, want: commitClosed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			authored := tt.name != "quoted"
			keys := tt.keys
			if len(keys) == 0 {
				keys = []string{"name:sam rivera"}
			}
			got := applyCommitmentLifecycle([]Commitment{lifecycleTestCommitment()}, []commitmentEvidence{{
				MemoryID:         "imessage_chat/closure",
				Text:             tt.text,
				OccurredAt:       "2026-07-20T11:00:00Z",
				Party:            tt.role,
				Authored:         authored,
				CounterpartyKeys: keys,
			}})
			if len(got) != 1 || got[0].State != tt.want {
				t.Fatalf("state = %+v, want %s", got, tt.want)
			}
		})
	}
}

func TestCommitmentClosurePreservesOpeningCitation(t *testing.T) {
	commitment := lifecycleTestCommitment()
	got := applyCommitmentLifecycle([]Commitment{commitment}, []commitmentEvidence{{
		MemoryID:         "imessage_chat/closure",
		Text:             "I sent the reviewer list.",
		OccurredAt:       "2026-07-20T11:00:00Z",
		Party:            commitmentPartySelf,
		Authored:         true,
		Citation:         mustLifecycleCitation("imessage_chat/closure", "2026-07-20T11:00:00Z"),
		CounterpartyKeys: []string{"name:sam rivera"},
	}})
	if len(got) != 1 {
		t.Fatalf("commitments = %d, want 1", len(got))
	}
	closed := got[0]
	if closed.State != commitClosed || closed.ClosureRef != "imessage_chat/closure" {
		t.Fatalf("closure = state %q ref %q", closed.State, closed.ClosureRef)
	}
	if len(closed.Citations) != 2 {
		t.Fatalf("citations = %+v, want opener plus closure", closed.Citations)
	}
	if closed.Citations[0].Role != commitCitationOpener ||
		closed.Citations[0].Citation.MemoryID() != commitment.OpenedBy.MemoryID ||
		closed.Citations[1].Role != commitCitationClosure ||
		closed.Citations[1].Citation.MemoryID() != "imessage_chat/closure" {
		t.Fatalf("citation order/roles = %+v", closed.Citations)
	}
}

func TestCommitmentSupersededIsNotClosed(t *testing.T) {
	got := applyCommitmentLifecycle([]Commitment{lifecycleTestCommitment()}, []commitmentEvidence{{
		MemoryID:         "gmail_thread/replacement",
		Text:             "The reviewer list deadline moved to Monday instead.",
		OccurredAt:       "2026-07-20T11:00:00Z",
		Party:            commitmentPartyCounterparty,
		Authored:         true,
		CounterpartyKeys: []string{"name:sam rivera"},
	}})
	if got[0].State != commitSuperseded || got[0].SupersededBy != "gmail_thread/replacement" {
		t.Fatalf("superseded transition = %+v", got[0])
	}
	if got[0].ClosureRef != commitClosureNone {
		t.Fatalf("superseded commitment got closure_ref %q", got[0].ClosureRef)
	}
}

func TestCommitmentClosureRefusesMultipleCandidates(t *testing.T) {
	first := lifecycleTestCommitment()
	second := lifecycleTestCommitment()
	second.ID = commitmentID("gmail_thread/second#m1", "body", 0)
	second.OpenedBy.MemoryID = "gmail_thread/second"
	second.OpenedBy.MessageRef = "gmail_thread/second#m1"
	second.Citations[0].CommitmentID = second.ID

	got := applyCommitmentLifecycle([]Commitment{first, second}, []commitmentEvidence{{
		MemoryID:         "imessage_chat/closure",
		Text:             "I sent the reviewer list.",
		OccurredAt:       "2026-07-20T11:00:00Z",
		Party:            commitmentPartySelf,
		Authored:         true,
		CounterpartyKeys: []string{"name:sam rivera"},
	}})
	for _, commitment := range got {
		if commitment.State != commitOpen || commitment.Gap == "" {
			t.Fatalf("ambiguous closure guessed instead of gapping: %+v", got)
		}
	}
}

func TestDistinctObligationsWithSameTextAreNotMerged(t *testing.T) {
	first := lifecycleTestCommitment()
	second := lifecycleTestCommitment()
	second.ID = commitmentID("gmail_thread/second#m1", "body", 0)
	second.OpenedBy.MemoryID = "gmail_thread/second"
	second.OpenedBy.MessageRef = "gmail_thread/second#m1"
	second.Citations[0].CommitmentID = second.ID

	got := deduplicateCommitments([]Commitment{first, second})
	if len(got) != 2 || got[0].DuplicateOf != "" || got[1].DuplicateOf != "" {
		t.Fatalf("text-only candidate generation merged distinct obligations: %+v", got)
	}
}

func TestCommitmentDedupRequiresStrongProvenanceAndKeepsCopyCitation(t *testing.T) {
	canonical := lifecycleTestCommitment()
	copy := lifecycleTestCommitment()
	copy.ID = commitmentID("notes/copy#m1", "body", 0)
	copy.OpenedBy.MemoryID = "notes/copy"
	copy.OpenedBy.MessageRef = "notes/copy#m1"
	copy.OpenedBy.AncestorRefs = []string{canonical.OpenedBy.MessageRef}
	copy.Counterparty = govAtom{Provider: "imessage", Kind: atomHandle, Value: "+15550100123"}
	copy.CounterpartyKeys = []string{"name:sam rivera", "given:sam"}
	copy.Citations = []CommitmentCitation{{
		Citation:     mustLifecycleCitation("notes/copy", "2026-07-20T10:05:00Z"),
		CommitmentID: copy.ID,
		Role:         commitCitationOpener,
	}}

	got := deduplicateCommitments([]Commitment{copy, canonical})
	if len(got) != 2 {
		t.Fatalf("dedup inventory = %d, want canonical plus marked copy", len(got))
	}
	byID := map[string]Commitment{}
	for _, commitment := range got {
		byID[commitment.ID] = commitment
	}
	if byID[copy.ID].DuplicateOf != canonical.ID {
		t.Fatalf("copy duplicate_of = %q, want %q", byID[copy.ID].DuplicateOf, canonical.ID)
	}
	foundSupporting := false
	for _, citation := range byID[canonical.ID].Citations {
		if citation.Role == commitCitationSupporting && citation.Citation.MemoryID() == "notes/copy" {
			foundSupporting = true
		}
	}
	if !foundSupporting {
		t.Fatalf("canonical citations lost duplicate evidence: %+v", byID[canonical.ID].Citations)
	}
}

func TestCommitmentSnapshotCrossSurfaceDeterminism(t *testing.T) {
	cfg, event, at := seedExamHomeFromRoot(t, examFixtureV2Root)
	meeting, err := readCommitmentSnapshot(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	daily, err := readCommitmentSnapshot(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if meeting.Generation == "" || meeting.Generation != daily.Generation ||
		!reflect.DeepEqual(meeting.Commitments, daily.Commitments) {
		t.Fatalf("same index generation diverged by surface:\nmeeting=%+v\ndaily=%+v", meeting, daily)
	}
	brief, err := buildEventMeetingBrief(t.Context(), cfg, event.EventID, at, 0, meetingBriefDefaultPerGuest)
	if err != nil {
		t.Fatal(err)
	}
	for _, section := range brief.Sections {
		for _, line := range section.Lines {
			if line.CommitmentID != "" && (line.Lifecycle == "" || line.ClosureRef == "") {
				t.Fatalf("typed line omitted shared lifecycle state: %+v", line)
			}
		}
	}
}

func TestMaterializedCommitmentLinksCrossSourceClosure(t *testing.T) {
	cfg, _, _ := seedExamHomeFromRoot(t, examFixtureV2Root)
	snapshot, err := readCommitmentSnapshot(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	var receipt *Commitment
	for i := range snapshot.Commitments {
		if snapshot.Commitments[i].OpenedBy.MemoryID == "gmail_thread/v2-receipt-thread" {
			receipt = &snapshot.Commitments[i]
			break
		}
	}
	if receipt == nil {
		t.Fatal("paper receipt commitment was not materialized")
	}
	if receipt.State != commitClosed || receipt.ClosureRef != "imessage_chat/v2-receipt-ack" {
		t.Fatalf("cross-source lifecycle = %+v", *receipt)
	}
	if len(receipt.Citations) < 2 ||
		receipt.Citations[0].Role != commitCitationOpener ||
		receipt.Citations[len(receipt.Citations)-1].Role != commitCitationClosure {
		t.Fatalf("cross-source closure did not preserve opener and add closure: %+v", receipt.Citations)
	}
}

func TestStaleSourceMarksCommitmentStateUncertain(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	if err := saveSources(cfg, []Source{{
		Name: "gmail", Type: "gmail", Email: "self@example.com",
		Enabled: ptr(true), CreatedAt: "2026-07-01T00:00:00Z",
	}}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	seedSyncStatus(t, cfg, "gmail", now.Add(-25*time.Hour))
	memory := Memory{
		ID: "gmail_thread/open", Type: "email", Provider: "gmail",
		ProviderID: "open", Source: "open", CreatedAt: "2026-07-20T10:00:00Z",
		Title: "Reviewer list",
		Text:  "From: Sam <sam@example.org>\n\nCan you send the reviewer list?",
		Meta: map[string]any{
			"from": []string{"sam@example.org"}, "to": []string{"self@example.com"},
			"occurred_at": "2026-07-20T10:00:00Z",
		},
	}
	got := materializeCommitments([]Memory{memory}, cfg, now)
	if len(got) != 1 || !got[0].StateUncertain || got[0].State != commitOpen {
		t.Fatalf("stale source silently asserted state: %+v", got)
	}
}
