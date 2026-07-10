package mora

import (
	"context"
	"strings"
	"testing"

	"github.com/pyranthus-hq/mora/internal/memory"
)

// imsgNamedMM is a 1:1 iMessage MappedMemory whose handle carries an address-book name.
func imsgNamedMM(guid, handle, name string) memory.MappedMemory {
	return memory.MappedMemory{
		StableID: "imessage_chat/" + guid, Provider: "imessage", Type: "imessage",
		ProviderID: guid, Source: guid, Title: "Chat " + guid, Body: "hi",
		ContentHash: "h_" + guid, Scope: "personal", CreatedAt: "2026-01-01T00:00:00Z",
		Meta: map[string]any{"participants": []map[string]string{{"handle": handle, "name": name}}, "message_count": "1"},
	}
}

// gmailNamedMM is an email MappedMemory FROM `from` (self-presenting `name`).
func gmailNamedMM(id, from, name, to string) memory.MappedMemory {
	return memory.MappedMemory{
		StableID: "gmail_thread/" + id, Provider: "gmail", Type: "email",
		ProviderID: id, Source: id, Title: "Subj " + id, Body: "b",
		ContentHash: "h_" + id, Scope: "personal", CreatedAt: "2026-01-01T00:00:00Z",
		Meta: map[string]any{"from": []string{from}, "to": []string{to}, "names": map[string]string{from: name}},
	}
}

func aliasesOf(t *testing.T, cfg Config, name string) []string {
	t.Helper()
	raw, err := graphGetEntity(context.Background(), cfg, name)
	if err != nil {
		t.Fatalf("graphGetEntity(%q): %v", name, err)
	}
	if raw["found"] != true {
		return nil
	}
	al, _ := raw["aliases"].([]string)
	return al
}

// TestMergeConfirmUnifiesEmailPhone is the end-to-end DONE criterion: a real
// email<->phone unification. The candidate is surfaced by `mora merge list`, and a
// one-tap `mora merge confirm` collapses the phone contact and the email into one
// person carrying both identities.
func TestMergeConfirmUnifiesEmailPhone(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	if err := writeMappedMemory(cfg, imsgNamedMM("solo", "+14155550123", "Riya Sharma")); err != nil {
		t.Fatal(err)
	}
	if err := writeMappedMemory(cfg, gmailNamedMM("t1", "riya@acme.com", "Riya Sharma", "me@x.com")); err != nil {
		t.Fatal(err)
	}

	// The queue surfaces the corroborated candidate (address-book name + address echo).
	out := run(t, "merge", "list")
	if !strings.Contains(out, "riya@acme.com") || !strings.Contains(out, "+14155550123") {
		t.Fatalf("merge list should surface the email<->phone candidate; got:\n%s", out)
	}

	// One-tap confirm -> rebuild -> the two identities are now one person.
	run(t, "merge", "confirm", "--handle", "+14155550123", "--email", "riya@acme.com")

	al := aliasesOf(t, cfg, "riya@acme.com")
	if !contains(al, "+14155550123") || !contains(al, "riya@acme.com") {
		t.Fatalf("confirmed merge did not unify identities; aliases=%v", al)
	}
	// The reverse lookup resolves to the same unified person.
	if al2 := aliasesOf(t, cfg, "+14155550123"); !contains(al2, "riya@acme.com") {
		t.Fatalf("phone lookup did not resolve to the unified person; aliases=%v", al2)
	}
	// The candidate is no longer pending (it graduated to a merge).
	if out := run(t, "merge", "list"); strings.Contains(out, "riya@acme.com") {
		t.Fatalf("confirmed pair must leave the queue; got:\n%s", out)
	}
}

// TestMergeRejectKeepsSeparate proves a rejected pair stays UNMERGED and is never
// re-proposed — refuse-to-gap, made durable.
func TestMergeRejectKeepsSeparate(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	if err := writeMappedMemory(cfg, imsgNamedMM("solo", "+14155550123", "Riya Sharma")); err != nil {
		t.Fatal(err)
	}
	if err := writeMappedMemory(cfg, gmailNamedMM("t1", "riya@acme.com", "Riya Sharma", "me@x.com")); err != nil {
		t.Fatal(err)
	}

	run(t, "merge", "reject", "--handle", "+14155550123", "--email", "riya@acme.com")

	if out := run(t, "merge", "list"); strings.Contains(out, "riya@acme.com") {
		t.Fatalf("rejected pair must not be re-proposed; got:\n%s", out)
	}
	if al := aliasesOf(t, cfg, "riya@acme.com"); contains(al, "+14155550123") {
		t.Fatalf("rejected pair must remain separate; aliases=%v", al)
	}
}

// TestMergeUndoRestoresQueue proves undo revokes a decision so the candidate returns.
func TestMergeUndoRestoresQueue(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	if err := writeMappedMemory(cfg, imsgNamedMM("solo", "+14155550123", "Riya Sharma")); err != nil {
		t.Fatal(err)
	}
	if err := writeMappedMemory(cfg, gmailNamedMM("t1", "riya@acme.com", "Riya Sharma", "me@x.com")); err != nil {
		t.Fatal(err)
	}
	run(t, "merge", "reject", "--handle", "+14155550123", "--email", "riya@acme.com")
	g, _ := loadGovernance(cfg)
	if len(g.Entries) != 1 {
		t.Fatalf("want 1 ledger entry, got %d", len(g.Entries))
	}
	run(t, "merge", "undo", g.Entries[0].ID)
	if out := run(t, "merge", "list"); !strings.Contains(out, "riya@acme.com") {
		t.Fatalf("undo should restore the pending candidate; got:\n%s", out)
	}
}

// TestMergeDecisionsResolvesAtoms pins the ledger→graph bridge: a merge_confirm
// entry keyed on source atoms resolves to the two pre-merge person ids, and a later
// reject supersedes an earlier confirm (last-writer-wins).
func TestMergeDecisionsResolvesAtoms(t *testing.T) {
	phone := govAtom{Provider: "imessage", Kind: atomHandle, Value: "+14155550123"}
	email := govAtom{Provider: "", Kind: atomAddress, Value: "riya@acme.com"}
	g := governance{Schema: governanceSchema, Entries: []govEntry{
		{ID: "gov_1", Kind: govKindMergeConfirm, Action: govActionRecord, Atom: phone, Atom2: &email, Decision: mergeDecisionConfirm},
	}}
	confirmed, decided := g.mergeDecisions()
	if len(confirmed) != 1 || confirmed[0].A == confirmed[0].B {
		t.Fatalf("want 1 confirmed pair, got %+v", confirmed)
	}
	if confirmed[0].A != personID("+14155550123") && confirmed[0].B != personID("+14155550123") {
		t.Fatalf("confirmed pair did not resolve to the phone person id: %+v", confirmed[0])
	}
	if !decided[mergePairKey(personID("+14155550123"), personID("riya@acme.com"))] {
		t.Fatal("pair should be marked decided")
	}

	// A later reject on the same pair supersedes the confirm.
	g.Entries = append(g.Entries, govEntry{ID: "gov_2", Kind: govKindMergeConfirm, Action: govActionRecord, Atom: phone, Atom2: &email, Decision: mergeDecisionReject})
	confirmed2, decided2 := g.mergeDecisions()
	if len(confirmed2) != 0 {
		t.Fatalf("reject must supersede confirm (last-writer-wins), got %+v", confirmed2)
	}
	if !decided2[mergePairKey(personID("+14155550123"), personID("riya@acme.com"))] {
		t.Fatal("a rejected pair is still decided (not re-proposed)")
	}
}

// TestMergeConfirmPersistsProvenance proves the confirmed fusion is durably recorded
// in the person_merges table for audit.
func TestMergeConfirmPersistsProvenance(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	if err := writeMappedMemory(cfg, imsgNamedMM("solo", "+14155550123", "Riya Sharma")); err != nil {
		t.Fatal(err)
	}
	if err := writeMappedMemory(cfg, gmailNamedMM("t1", "riya@acme.com", "Riya Sharma", "me@x.com")); err != nil {
		t.Fatal(err)
	}
	run(t, "merge", "confirm", "--handle", "+14155550123", "--email", "riya@acme.com")

	db, err := ensureIndexDB(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM person_merges WHERE signal = ?`, sigConfirmed).Scan(&n); err != nil {
		t.Fatalf("person_merges query: %v", err)
	}
	if n == 0 {
		t.Fatal("confirmed merge provenance was not persisted to person_merges")
	}
}
