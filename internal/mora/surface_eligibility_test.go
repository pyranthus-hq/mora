package mora

import (
	"testing"
	"time"
)

func TestFilterDigestInstancesByCommitmentsGatesBeforeAssembly(t *testing.T) {
	instances := map[string][]Memory{
		"gmail": {
			{ID: "open"},
			{ID: "closed"},
			{ID: "duplicate"},
			{ID: "not-a-commitment"},
		},
	}
	inventory := map[string][]Commitment{
		"open":      {{State: commitOpen}},
		"closed":    {{State: commitClosed}},
		"duplicate": {{State: commitOpen, DuplicateOf: "canonical"}},
	}

	got := filterDigestInstancesByCommitments(instances, inventory)
	if len(got["gmail"]) != 1 || got["gmail"][0].ID != "open" {
		t.Fatalf("filtered instances = %+v, want only the open canonical commitment artifact", got)
	}
	if len(instances["gmail"]) != 4 {
		t.Fatal("filter mutated its input")
	}
}

func TestDigestCommitmentForMultipleOpeningsUsesVisibleEvidence(t *testing.T) {
	first := Commitment{
		OpenedBy:  commitSpan{MemoryID: "thread", Quote: "I will reserve the west press slot.", OccurredAt: "2026-07-22T17:00:00Z"},
		State:     commitOpen,
		Direction: commitOwedByCounterparty,
	}
	second := Commitment{
		OpenedBy:  commitSpan{MemoryID: "thread", Quote: "Please send the route cards before breakfast.", OccurredAt: "2026-07-22T17:30:00Z"},
		State:     commitOpen,
		Direction: commitOwedBySelf,
	}
	item := DigestItem{Title: "Route cards", Snippet: "I will reserve the west press slot. Please send the route cards before breakfast."}

	got, ok := digestCommitmentFor(item, []Commitment{second, first})
	if !ok || got.Direction != commitOwedBySelf {
		t.Fatalf("selected commitment = %+v, want the last visible independently anchored opening", got)
	}
}

func TestAttachDigestCommitmentsNestsEveryIdentifiedOpening(t *testing.T) {
	first := Commitment{
		ID:        "commit:v1:first",
		Owner:     govAtom{Kind: "person", Value: "self@example.com"},
		Direction: commitOwedBySelf,
		Summary:   "send the route cards",
		OpenedBy: commitSpan{
			MemoryID: "gmail_thread/route-cards", Quote: "I will send the route cards.",
			OccurredAt: "2026-07-22T17:00:00Z",
		},
		Due:        commitDue{Kind: commitDueRelative},
		State:      commitOpen,
		ClosureRef: commitClosureNone,
	}
	first.Citations = []CommitmentCitation{{
		Citation:     mustLifecycleCitation(first.OpenedBy.MemoryID, first.OpenedBy.OccurredAt),
		CommitmentID: first.ID,
		Role:         commitCitationOpener,
	}}
	second := Commitment{
		ID:        "commit:v1:second",
		Owner:     govAtom{Kind: "person", Value: "theo@example.org"},
		Direction: commitOwedByCounterparty,
		Summary:   "reserve the west press slot",
		OpenedBy: commitSpan{
			MemoryID: "gmail_thread/route-cards", Quote: "I will reserve the west press slot.",
			OccurredAt: "2026-07-22T17:30:00Z",
		},
		Due:        commitDue{Kind: commitDueNone},
		State:      commitOpen,
		ClosureRef: commitClosureNone,
	}
	second.Citations = []CommitmentCitation{{
		Citation:     mustLifecycleCitation(second.OpenedBy.MemoryID, second.OpenedBy.OccurredAt),
		CommitmentID: second.ID,
		Role:         commitCitationOpener,
	}}
	uncited := second
	uncited.ID = "commit:v1:uncited"
	uncited.Citations = nil
	legacy := second
	legacy.ID = ""
	legacy.Citations = nil

	digest := Digest{Sections: []DigestSection{{
		Source: "gmail",
		Items:  []DigestItem{{ID: "gmail_thread/route-cards", Title: "Route cards"}},
	}}}
	attachDigestCommitments(&digest, map[string][]Commitment{
		"gmail_thread/route-cards": {legacy, uncited, second, first},
	})

	if len(digest.Sections) != 1 || len(digest.Sections[0].Items) != 1 {
		t.Fatalf("commitment attachment changed artifact selection: %+v", digest.Sections)
	}
	item := digest.Sections[0].Items[0]
	if item.Direction != "" {
		t.Fatalf("identified artifact also received the legacy scalar lane: %+v", item)
	}
	if len(item.Obligations) != 2 {
		t.Fatalf("identified obligation rows = %+v, want the two cited commitments only", item.Obligations)
	}
	if item.Obligations[0].CommitmentID != first.ID ||
		item.Obligations[1].CommitmentID != second.ID {
		t.Fatalf("obligation evidence order = %+v", item.Obligations)
	}
	for _, obligation := range item.Obligations {
		if len(obligation.Citations) != 1 ||
			obligation.Citations[0].Role != commitCitationOpener ||
			obligation.Citations[0].CommitmentID != obligation.CommitmentID ||
			obligation.Citations[0].Citation.MemoryID() != item.ID {
			t.Fatalf("obligation lost its own typed opening citation: %+v", obligation)
		}
	}
}

func TestDigestCommitmentForRejectsClosedAndDuplicateOnlyArtifact(t *testing.T) {
	item := DigestItem{ID: "thread"}
	_, ok := digestCommitmentFor(item, []Commitment{
		{State: commitClosed},
		{State: commitOpen, DuplicateOf: "canonical"},
	})
	if ok {
		t.Fatal("closed/duplicate-only artifact was eligible")
	}
}

func TestCommitmentDailyEligibleUsesOpeningEvidenceAndInclusiveSevenDayWindow(t *testing.T) {
	at := time.Date(2026, 7, 23, 18, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		c    Commitment
		want bool
	}{
		{
			name: "inclusive cutoff",
			c:    Commitment{State: commitOpen, OpenedBy: commitSpan{OccurredAt: at.Add(-commitmentDailyWindow).Format(time.RFC3339)}},
			want: true,
		},
		{
			name: "inclusive as-of",
			c:    Commitment{State: commitOpen, OpenedBy: commitSpan{OccurredAt: at.Format(time.RFC3339)}},
			want: true,
		},
		{
			name: "one instant too old",
			c:    Commitment{State: commitOpen, OpenedBy: commitSpan{OccurredAt: at.Add(-commitmentDailyWindow - time.Second).Format(time.RFC3339)}},
			want: false,
		},
		{
			name: "future opening",
			c:    Commitment{State: commitOpen, OpenedBy: commitSpan{OccurredAt: at.Add(time.Second).Format(time.RFC3339)}},
			want: false,
		},
		{
			name: "closed recent",
			c:    Commitment{State: commitClosed, OpenedBy: commitSpan{OccurredAt: at.Format(time.RFC3339)}},
			want: false,
		},
		{
			name: "duplicate recent",
			c:    Commitment{State: commitOpen, DuplicateOf: "canonical", OpenedBy: commitSpan{OccurredAt: at.Format(time.RFC3339)}},
			want: false,
		},
		{
			name: "missing evidence time",
			c:    Commitment{State: commitOpen},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := commitmentDailyEligible(tt.c, at); got != tt.want {
				t.Fatalf("eligible = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCommitmentRefersToMeetingRequiresExplicitReference(t *testing.T) {
	event := Memory{
		Title: "Atrium handoff session",
		Text:  "Agenda: entry flow, neighborhood diagram, and supply shelves.",
	}
	tests := []struct {
		name    string
		summary string
		quote   string
		want    bool
	}{
		{
			name:    "full title",
			summary: "Bring the map to the Atrium handoff session",
			want:    true,
		},
		{
			name:  "multiple distinctive agenda terms",
			quote: "Please bring the neighborhood diagram.",
			want:  true,
		},
		{
			name:  "definite agenda reference",
			quote: "I will bring it to the session.",
			want:  true,
		},
		{
			name:  "incidental attendee history",
			quote: "Please log the cooling readings before evening changeover.",
			want:  false,
		},
		{
			name:  "one incidental shared token",
			quote: "Please print the Atrium cards.",
			want:  false,
		},
		{
			name:  "two stopword-only overlaps",
			quote: "This is for you.",
			want:  false,
		},
		{
			name:  "definite non-referent title token",
			quote: "Please label the Atrium crate.",
			want:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Commitment{Summary: tt.summary, OpenedBy: commitSpan{Quote: tt.quote}}
			if got := commitmentRefersToMeeting(c, event, nil); got != tt.want {
				t.Fatalf("refers = %v, want %v", got, tt.want)
			}
		})
	}
}
