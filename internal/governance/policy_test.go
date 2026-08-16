package governance

import (
	"github.com/pyranthus-hq/mora/internal/memory"
	"testing"
)

func gmailMemory(id string, from, to []string) memory.MappedMemory {
	return memory.MappedMemory{Provider: "gmail", StableID: "gmail_thread/" + id, Meta: map[string]any{"from": from, "to": to}}
}
func imessageMemory(id string, handles ...string) memory.MappedMemory {
	parts := make([]any, 0, len(handles))
	for _, h := range handles {
		parts = append(parts, map[string]any{"handle": h})
	}
	return memory.MappedMemory{Provider: "imessage", StableID: "imessage_chat/" + id, Meta: map[string]any{"participants": parts}}
}
func TestGovernance_SuppressItemAtomExactOnly(t *testing.T) {
	g := Ledger{Schema: 1, Entries: []Entry{{ID: "e1", Kind: KindForget, Action: ActionSuppress, Atom: Atom{Kind: AtomStableID, Value: "gmail_thread/x"}}}}
	if ok, _ := Suppresses(g, gmailMemory("x", nil, nil)); !ok {
		t.Fatal("exact")
	}
	if ok, _ := Suppresses(g, gmailMemory("x@work", nil, nil)); ok {
		t.Fatal("account overmatch")
	}
	if ok, _ := Suppresses(g, gmailMemory("y", nil, nil)); ok {
		t.Fatal("unrelated")
	}
}
func TestGovernance_SuppressSoleHandleButKeepGroup(t *testing.T) {
	g := Ledger{Entries: []Entry{{ID: "e1", Kind: KindForget, Action: ActionSuppress, Atom: Atom{Provider: "imessage", Kind: AtomHandle, Value: "+14155550123"}}}}
	if ok, _ := Suppresses(g, imessageMemory("solo", "+14155550123")); !ok {
		t.Fatal("sole")
	}
	if ok, _ := Suppresses(g, imessageMemory("group", "+14155550123", "+14155550999")); ok {
		t.Fatal("group")
	}
}
func TestGovernance_GmailRealisticOneToOneNotSuppressed(t *testing.T) {
	g := Ledger{Entries: []Entry{{ID: "e1", Kind: KindForget, Action: ActionSuppress, Atom: Atom{Kind: AtomAddress, Value: "sam@example.com"}}}}
	if ok, _ := Suppresses(g, gmailMemory("t", []string{"sam@example.com"}, []string{"me@x.com"})); ok {
		t.Fatal("self-included email")
	}
}
func TestGovernance_BriefLineDecisionsPersistAndLastWriterWins(t *testing.T) {
	stable := Atom{Provider: "gmail", Kind: AtomStableID, Value: "gmail_thread/t1"}
	att := Atom{Kind: AtomAddress, Value: " SAM@example.com "}
	g := Ledger{Entries: []Entry{{Kind: KindRedact, Action: ActionRecord, Atom: stable, Atom2: &att, Decision: DecisionReject}, {Kind: KindRedact, Action: ActionRecord, Atom: stable, Atom2: &att, Decision: DecisionConfirm}}}
	normalized := att
	normalized.Value = "sam@example.com"
	if got := BriefLineDecisions(g)[BriefLineDecisionKey(stable, normalized)]; got != DecisionConfirm {
		t.Fatalf("got=%q", got)
	}
}
func TestGovernanceParentContextDeduplicatesAndSuppressesDerived(t *testing.T) {
	meta := map[string]any{ParentProviderKey: "gmail", ParentStableIDKey: "gmail_thread/p", ParentAtomsKey: []any{map[string]any{"kind": "address", "value": " A@EXAMPLE.COM "}, map[string]string{"kind": "address", "value": "a@example.com"}}}
	p, id, atoms := ParentContext(meta)
	if p != "gmail" || id != "gmail_thread/p" || len(atoms) != 1 || atoms[0].Value != "a@example.com" {
		t.Fatalf("%s %s %+v", p, id, atoms)
	}
	g := Ledger{Entries: []Entry{{ID: "f", Kind: KindForget, Action: ActionSuppress, Atom: Atom{Provider: "gmail", Kind: AtomStableID, Value: "gmail_thread/p"}}}}
	if ok, why := DecideSuppress(g, "filesystem", "attachment/x", meta); !ok || why != "f" {
		t.Fatalf("ok=%v why=%q", ok, why)
	}
}
func TestGovernanceMergeDecisionsLatestWinsAndSorts(t *testing.T) {
	a := Atom{Kind: AtomAddress, Value: "b@example.com"}
	b := Atom{Kind: AtomAddress, Value: "a@example.com"}
	g := Ledger{Entries: []Entry{{ID: "1", Kind: KindMergeConfirm, Atom: a, Atom2: &b, Decision: DecisionReject}, {ID: "2", Kind: KindMergeConfirm, Atom: a, Atom2: &b, Decision: DecisionConfirm}}}
	confirmed, decided := MergeDecisions(g)
	if len(confirmed) != 1 || confirmed[0].GovID != "2" || len(decided) != 1 {
		t.Fatalf("confirmed=%+v decided=%+v", confirmed, decided)
	}
}
func TestGovernanceTeachProjections(t *testing.T) {
	g := Ledger{Entries: []Entry{{ID: "m", Kind: KindTeachMemory, Action: ActionRecord, Decision: "supersede", TargetID: "old", ReplacementID: "new"}, {ID: "c", Kind: KindTeachCommitment, Action: ActionRecord, Decision: "useful"}, {ID: "e1", Kind: KindEvalConsent, Action: ActionRecord, Decision: "enable"}, {ID: "e2", Kind: KindEvalConsent, Action: ActionRecord, Decision: "disable", RevokedAt: "x"}}}
	if MemoryVisible(g, "old") || !MemoryVisible(g, "new") {
		t.Fatal("visibility")
	}
	if len(TeachingEntries(g)) != 4 || len(ActiveTeachCommitments(g)) != 1 || !EvalConsentEnabled(g) {
		t.Fatalf("teach=%d commitments=%d consent=%v", len(TeachingEntries(g)), len(ActiveTeachCommitments(g)), EvalConsentEnabled(g))
	}
}
