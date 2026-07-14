package exam

import (
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// The twelve hand-written rejecting rows can only SAMPLE the space of broken
// ledgers. These two properties cover the space: the validator accepts every
// well-formed ledger a generator can build, and rejects every one-field corruption
// of one. A minimized counterexample from here is promoted into the committed
// ledger — the property test finds it once; the ledger owns it forever.
//
// Generation stays outside the scorer. The AST determinism guard fails the build if
// rapid is ever imported anywhere but a *_prop_test.go file, because a PRNG on a
// scoring path would quietly destroy every byte-stability promise the exam makes.

func genLedger(t *rapid.T) Ledger {
	return GenerateLedger(func(label string, min, max int) int {
		return rapid.IntRange(min, max).Draw(t, label)
	})
}

func TestPropValidatorAcceptsWellFormedLedgers(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		l := genLedger(t)
		if err := Validate(l); err != nil {
			t.Fatalf("Validate rejected a well-formed generated ledger: %v", err)
		}
		if err := Lint(l); err != nil {
			t.Fatalf("Lint rejected a generated ledger built from reserved identifiers: %v", err)
		}
	})
}

// TestPropValidatorRejectsEveryOneFieldCorruption is the property the twelve
// hand-written rows can only sample: pick a well-formed ledger, break exactly ONE
// field, and the validator must refuse it BY NAME. A corruption that slips through
// is a rule the exam does not actually have.
func TestPropValidatorRejectsEveryOneFieldCorruption(t *testing.T) {
	corruptions := []struct {
		rule    string
		corrupt func(*Ledger)
	}{
		{RuleIdentity, func(l *Ledger) { l.Commitments[0].Owner = "p/ghost" }},
		{RuleIdentity, func(l *Ledger) { l.Artifacts[1].Messages[0].From = "p/ghost" }},
		{RuleTimestamp, func(l *Ledger) { l.Artifacts[1].Messages[0].At = "yesterday" }},
		{RuleTimestamp, func(l *Ledger) { l.Artifacts[1].OccurredAt = "2027-01-01T00:00:00Z" }},
		{RuleDirection, func(l *Ledger) { l.Commitments[0].Direction = DirectionOwedByCounterparty }},
		{RuleTransition, func(l *Ledger) { l.Commitments[4].Transitions = nil }},
		{RuleEvidenceSpan, func(l *Ledger) { l.Commitments[0].OpenedBy.Quote = "a sentence that is nowhere" }},
		{RuleEvidenceSpan, func(l *Ledger) { l.Commitments[0].OpenedBy.BlockID = "b9" }},
		{RuleEvidenceSpan, func(l *Ledger) { l.Artifacts[2].MemoryID = l.Artifacts[1].MemoryID }},
		{RuleEvidenceSpan, func(l *Ledger) {
			l.Artifacts[1].Messages[0].Body[0].ID = "b1x"
			l.Commitments[0].OpenedBy.BlockID = "b1"
		}},
		{RuleClosure, func(l *Ledger) {
			l.Commitments[4].Transitions[0].Evidence = Span{ArtifactID: "a/chat", MessageID: "m1", BlockID: "b1", Quote: "I will send the report."}
		}},
		{RuleSelfAttendee, func(l *Ledger) { l.Artifacts[0].Messages[0].To = []string{l.People[0].ID} }},
		{RuleOneDefectArtifact, func(l *Ledger) { l.NonObligations[0].Class = "vibes" }},
		{RuleOneDefectArtifact, func(l *Ledger) {
			l.NonObligations = append(l.NonObligations, NonObligation{ID: "n/second", Class: "footer", Why: "second defect",
				Span: Span{ArtifactID: "a/neg", MessageID: "m1", BlockID: "b1", Quote: "Nothing is being asked of you here."}})
		}},
		{RuleClassBalance, func(l *Ledger) {
			for i := range l.Commitments {
				l.Commitments[i].Owner, l.Commitments[i].Counterparty = l.Self.ID, l.People[0].ID
				l.Commitments[i].Direction = DirectionOwedBySelf
			}
		}},
		{RulePersonaHygiene, func(l *Ledger) { l.People[0].Emails = []string{"real@acme.com"} }},
		{RulePersonaHygiene, func(l *Ledger) { l.People[0].Handles = []string{"+14155550123"} }},
		{RuleChannelGrain, func(l *Ledger) {
			// APPENDED, not overwritten: overwriting a block that carries a gold span
			// would break the span rule too, and a corruption that trips two rules
			// cannot prove which one is load-bearing.
			l.Artifacts[1].Messages[0].Body = append(l.Artifacts[1].Messages[0].Body,
				Block{ID: "b2", Kind: "authored", Text: "> a quoted line the connector would have stripped"})
		}},
		{RuleChannelGrain, func(l *Ledger) {
			second := l.Artifacts[0].Messages[0]
			second.ID = "m2"
			l.Artifacts[0].Messages = append(l.Artifacts[0].Messages, second)
		}},
		{RuleChannelGrain, func(l *Ledger) { l.Artifacts[4].Messages[0].To = []string{l.Self.ID} }},
		{RuleReplyChainQuotes, func(l *Ledger) {
			for ai := range l.Artifacts {
				for mi := range l.Artifacts[ai].Messages {
					for bi := range l.Artifacts[ai].Messages[mi].Body {
						if l.Artifacts[ai].Messages[mi].Body[bi].Kind == "quoted_reply" {
							l.Artifacts[ai].Messages[mi].Body[bi].Kind = "authored"
						}
					}
				}
			}
		}},
	}
	rapid.Check(t, func(t *rapid.T) {
		i := rapid.IntRange(0, len(corruptions)-1).Draw(t, "corruption")
		c := corruptions[i]
		l := genLedger(t)
		if err := Validate(l); err != nil {
			t.Fatalf("the generator produced an invalid ledger before any corruption: %v", err)
		}
		c.corrupt(&l)
		err := Validate(l)
		if err == nil {
			t.Fatalf("Validate ACCEPTED a ledger corrupted at rule %q", c.rule)
		}
		if !strings.Contains(err.Error(), c.rule) {
			t.Fatalf("corruption of rule %q was rejected as %v — the refusal must name the rule it belongs to", c.rule, err)
		}
	})
}
