package mora

import "testing"

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
