package mora

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/pyranthus-hq/mora/internal/memory"
)

// ---------------------------------------------------------------------------
// Test fixtures: MappedMemory in the exact shape each connector produces.
// ---------------------------------------------------------------------------

func imsgMM(guid string, handles ...string) memory.MappedMemory {
	pairs := make([]map[string]string, 0, len(handles))
	for _, h := range handles {
		pairs = append(pairs, map[string]string{"handle": h, "name": ""})
	}
	return memory.MappedMemory{
		StableID: "imessage_chat/" + guid, Provider: "imessage", Type: "imessage",
		ProviderID: guid, Source: guid, Title: "Chat " + guid, Body: "hi",
		ContentHash: "h_" + guid, Scope: "personal", CreatedAt: "2026-01-01T00:00:00Z",
		Meta: map[string]any{"participants": pairs, "message_count": "1"},
	}
}

func gmailMM(id string, from, to []string) memory.MappedMemory {
	meta := map[string]any{"message_count": "1"}
	if len(from) > 0 {
		meta["from"] = from
	}
	if len(to) > 0 {
		meta["to"] = to
	}
	return memory.MappedMemory{
		StableID: "gmail_thread/" + id, Provider: "gmail", Type: "email",
		ProviderID: id, Source: id, Title: "Subj " + id, Body: "b",
		ContentHash: "h_" + id, Scope: "personal", CreatedAt: "2026-01-01T00:00:00Z",
		Meta: meta,
	}
}

// ---------------------------------------------------------------------------
// atom derivation (pure, the Meta->key map)
// ---------------------------------------------------------------------------

func TestGovernance_ItemAtomIsExactStableID(t *testing.T) {
	a := itemAtom("gmail", "gmail_thread/x@work")
	if a.Kind != atomStableID || a.Value != "gmail_thread/x@work" || a.Provider != "gmail" {
		t.Fatalf("item atom = %+v", a)
	}
}

func TestGovernance_CounterpartiesIMessageHandles(t *testing.T) {
	mm := imsgMM("g1", "+14155550123")
	cps := counterpartyAtoms(mm.Provider, mm.Meta)
	if len(cps) != 1 || cps[0].Kind != atomHandle || cps[0].Value != "+14155550123" || cps[0].Provider != "imessage" {
		t.Fatalf("counterparties = %+v", cps)
	}
}

func TestGovernance_CounterpartiesGmailAddressesLowercased(t *testing.T) {
	mm := gmailMM("t1", []string{"Sam@Example.com"}, []string{"me@x.com"})
	cps := counterpartyAtoms(mm.Provider, mm.Meta)
	// distinct addresses across from+to => 2, lowercased.
	got := map[string]bool{}
	for _, c := range cps {
		if c.Kind != atomAddress {
			t.Fatalf("want address kind, got %+v", c)
		}
		got[c.Value] = true
	}
	if !got["sam@example.com"] || !got["me@x.com"] || len(got) != 2 {
		t.Fatalf("counterparties = %+v", cps)
	}
}

// ---------------------------------------------------------------------------
// ledger load/save/append/revoke
// ---------------------------------------------------------------------------

func TestGovernance_LoadAbsentIsEmptyNoError(t *testing.T) {
	cfg := Config{VaultDir: t.TempDir()}
	g, err := loadGovernance(cfg)
	if err != nil {
		t.Fatalf("absent ledger must not error: %v", err)
	}
	if len(g.Entries) != 0 {
		t.Fatalf("absent ledger must be empty, got %d entries", len(g.Entries))
	}
}

func TestGovernance_CorruptFailsClosed(t *testing.T) {
	cfg := Config{VaultDir: t.TempDir()}
	if err := os.WriteFile(governancePath(cfg), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A corrupt governance ledger must FAIL LOUD, never be treated as empty — a
	// silently-ignored ledger would resurrect forgotten content (privacy leak).
	if _, err := loadGovernance(cfg); err == nil {
		t.Fatal("corrupt ledger must return an error, not an empty ledger")
	}
}

func TestGovernance_AppendRevokeRoundTrip(t *testing.T) {
	cfg := Config{VaultDir: t.TempDir()}
	e, err := appendGovernanceEntry(cfg, govEntry{
		Kind: govKindForget, Action: govActionSuppress,
		Atom: govAtom{Provider: "imessage", Kind: atomHandle, Value: "+14155550123"},
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if e.ID == "" || e.CreatedAt == "" {
		t.Fatalf("append must mint id + created_at, got %+v", e)
	}
	g, _ := loadGovernance(cfg)
	if len(g.activeSuppress()) != 1 {
		t.Fatalf("want 1 active suppress entry, got %d", len(g.activeSuppress()))
	}
	found, err := revokeGovernanceEntry(cfg, e.ID)
	if err != nil || !found {
		t.Fatalf("revoke: found=%v err=%v", found, err)
	}
	g, _ = loadGovernance(cfg)
	if len(g.activeSuppress()) != 0 {
		t.Fatalf("revoked entry must be inert, got %d active", len(g.activeSuppress()))
	}
}

// ---------------------------------------------------------------------------
// the suppression decision (guard core)
// ---------------------------------------------------------------------------

func TestGovernance_SuppressItemAtomExactOnly(t *testing.T) {
	g := governance{Schema: governanceSchema, Entries: []govEntry{{
		ID: "e1", Kind: govKindForget, Action: govActionSuppress,
		Atom: govAtom{Kind: atomStableID, Value: "gmail_thread/x"},
	}}}
	// exact match suppresses
	if sup, _ := g.suppresses(gmailMM("x", nil, nil)); !sup {
		t.Fatal("exact stable_id must be suppressed")
	}
	// @account variant must NOT be caught by a base-id forget (no @account strip).
	if sup, _ := g.suppresses(gmailMM("x@work", nil, nil)); sup {
		t.Fatal("forgetting base id must NOT suppress the @account thread (over-match)")
	}
	// a different thread is untouched
	if sup, _ := g.suppresses(gmailMM("y", nil, nil)); sup {
		t.Fatal("unrelated thread must not be suppressed")
	}
}

func TestGovernance_SuppressSoleHandleButKeepGroup(t *testing.T) {
	g := governance{Schema: governanceSchema, Entries: []govEntry{{
		ID: "e1", Kind: govKindForget, Action: govActionSuppress,
		Atom: govAtom{Provider: "imessage", Kind: atomHandle, Value: "+14155550123"},
	}}}
	// 1:1 chat with the forgotten handle => suppressed.
	if sup, _ := g.suppresses(imsgMM("solo", "+14155550123")); !sup {
		t.Fatal("1:1 chat with forgotten handle must be suppressed")
	}
	// GROUP chat containing the forgotten handle among others => KEPT (data-loss guard).
	if sup, _ := g.suppresses(imsgMM("grp", "+14155550123", "+14155550999")); sup {
		t.Fatal("group thread must NOT be whole-suppressed by a person forget")
	}
}

func TestGovernance_GmailSoleAddressOnly(t *testing.T) {
	g := governance{Schema: governanceSchema, Entries: []govEntry{{
		ID: "e1", Kind: govKindForget, Action: govActionSuppress,
		Atom: govAtom{Kind: atomAddress, Value: "sam@example.com"},
	}}}
	// A thread whose ONLY external address is the target => suppressed.
	if sup, _ := g.suppresses(gmailMM("solo", []string{"sam@example.com"}, nil)); !sup {
		t.Fatal("sole-address thread must be suppressed")
	}
	// A thread where the address is one of several => KEPT (self/other included).
	if sup, _ := g.suppresses(gmailMM("multi", []string{"sam@example.com"}, []string{"me@x.com"})); sup {
		t.Fatal("multi-address thread must not be whole-suppressed")
	}
}

// ---------------------------------------------------------------------------
// write-chokepoint guard: no resurrection across re-sync
// ---------------------------------------------------------------------------

func TestGovernance_WriteGuardBlocksResurrection(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	mm := imsgMM("solo", "+14155550123")
	dest := filepath.Join(sourcesRoot(cfg), "imessage", memory.SafeFilename(mm.StableID)+".md")

	// First sync writes the file.
	if err := writeMappedMemory(cfg, mm); err != nil {
		t.Fatalf("initial write: %v", err)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("expected file after first sync: %v", err)
	}
	// Remove it and record a suppression (what `mora forget` does).
	if err := os.Remove(dest); err != nil {
		t.Fatal(err)
	}
	if _, err := appendGovernanceEntry(cfg, govEntry{
		Kind: govKindForget, Action: govActionSuppress,
		Atom: govAtom{Provider: "imessage", Kind: atomHandle, Value: "+14155550123"},
	}); err != nil {
		t.Fatal(err)
	}
	// Next hourly sync re-fetches the live item; the guard must suppress the write.
	if err := writeMappedMemory(cfg, mm); err != nil {
		t.Fatalf("guarded write should be a no-op, got: %v", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("RESURRECTION: forgotten memory was re-created by the next sync")
	}
}

func TestGovernance_CorruptLedgerAbortsWrite(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	if err := os.WriteFile(governancePath(cfg), []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A corrupt ledger must abort the write (honest-snapshot), never silently
	// resurrect by treating the ledger as empty.
	if err := writeMappedMemory(cfg, imsgMM("solo", "+1")); err == nil {
		t.Fatal("corrupt ledger must abort the connector write")
	}
}

func TestGovernance_AttachmentInheritsParentSuppression(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	parent := imsgMM("solo", "+14155550123")
	if _, err := appendGovernanceEntry(cfg, govEntry{
		Kind: govKindForget, Action: govActionSuppress,
		Atom: govAtom{Provider: "imessage", Kind: atomHandle, Value: "+14155550123"},
	}); err != nil {
		t.Fatal(err)
	}
	// A forgotten chat's derived PDF attachments must not slip through the 5th
	// (derived) write path.
	parent.Attachments = []memory.Attachment{{Path: "/tmp/does-not-matter.pdf", MimeType: "application/pdf", Filename: "x.pdf"}}
	n, err := writeAttachmentMemories(cfg, parent)
	if err != nil {
		t.Fatalf("writeAttachmentMemories: %v", err)
	}
	if n != 0 {
		t.Fatalf("suppressed parent must write 0 attachment memories, wrote %d", n)
	}
}

// ---------------------------------------------------------------------------
// merge_confirm correction persists across re-sync
// ---------------------------------------------------------------------------

func TestGovernance_MergeConfirmPersists(t *testing.T) {
	cfg := Config{VaultDir: t.TempDir()}
	// A human correction: this handle and this address are the SAME person.
	// Keyed by stable-atom identities, NEVER a post-merge person: id.
	e, err := appendGovernanceEntry(cfg, govEntry{
		Kind: govKindMergeConfirm, Action: govActionRecord, Decision: "confirm",
		Atom:  govAtom{Provider: "imessage", Kind: atomHandle, Value: "+14155550123"},
		Atom2: &govAtom{Provider: "gmail", Kind: atomAddress, Value: "sam@example.com"},
	})
	if err != nil {
		t.Fatalf("append merge_confirm: %v", err)
	}
	// Reload (simulating a fresh process after the next connector sync).
	g, err := loadGovernance(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var got *govEntry
	for i := range g.Entries {
		if g.Entries[i].ID == e.ID {
			got = &g.Entries[i]
		}
	}
	if got == nil {
		t.Fatal("correction did not survive reload/re-sync")
	}
	if got.Decision != "confirm" || got.Atom2 == nil || got.Atom2.Value != "sam@example.com" {
		t.Fatalf("correction lost its stable-atom keys: %+v", got)
	}
}

func TestGovernance_BriefLineDecisionsPersistAndLastWriterWins(t *testing.T) {
	cfg := Config{VaultDir: t.TempDir()}
	stable := govAtom{Provider: "gmail", Kind: atomStableID, Value: "gmail_thread/t1"}
	attendee := govAtom{Kind: atomAddress, Value: "sam@example.com"}
	if _, err := appendGovernanceEntry(cfg, govEntry{
		Kind:     govKindRedact,
		Action:   govActionRecord,
		Atom:     stable,
		Atom2:    &attendee,
		Decision: mergeDecisionReject,
	}); err != nil {
		t.Fatalf("append reject: %v", err)
	}
	if _, err := appendGovernanceEntry(cfg, govEntry{
		Kind:     govKindRedact,
		Action:   govActionRecord,
		Atom:     stable,
		Atom2:    &attendee,
		Decision: mergeDecisionConfirm,
	}); err != nil {
		t.Fatalf("append confirm: %v", err)
	}
	g, err := loadGovernance(cfg)
	if err != nil {
		t.Fatal(err)
	}
	key := briefLineDecisionKey(stable, attendee)
	if got := g.briefLineDecisions()[key]; got != mergeDecisionConfirm {
		t.Fatalf("brief line decision = %q, want last-writer %q", got, mergeDecisionConfirm)
	}
}

// ---------------------------------------------------------------------------
// durability: rides `mora sync git`, ignored by index rebuild
// ---------------------------------------------------------------------------

func TestGovernance_LedgerNotGitIgnored(t *testing.T) {
	// The ledger must survive `mora sync git` (#52: consistent across a user's
	// own devices) — the vault .gitignore must not exclude it.
	base := filepath.Base(governancePath(Config{VaultDir: "/v"}))
	for _, pat := range strings.Split(gitignoreBody, "\n") {
		pat = strings.TrimSpace(pat)
		if pat == "" || strings.HasPrefix(pat, "#") {
			continue
		}
		if pat == base || (strings.HasPrefix(pat, "*.") && strings.HasSuffix(base, pat[1:])) {
			t.Fatalf("governance ledger %q is excluded by .gitignore pattern %q", base, pat)
		}
	}
}

func TestGovernance_LedgerDotfileIgnoredByRebuild(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	// One real connector memory + a governance dotfile in the vault.
	if err := writeMappedMemory(cfg, gmailMM("keep", []string{"a@x.com"}, nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := appendGovernanceEntry(cfg, govEntry{
		Kind: govKindForget, Action: govActionSuppress,
		Atom: govAtom{Kind: atomStableID, Value: "gmail_thread/other"},
	}); err != nil {
		t.Fatal(err)
	}
	n, err := rebuildIndexWithPolicy(context.Background(), cfg, policyAllow)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if n != 1 {
		t.Fatalf("rebuild must index exactly the 1 memory (ledger dotfile ignored), got %d", n)
	}
}

// ---------------------------------------------------------------------------
// the FIFTH (filesystem) write path: ingestFilesystem renders directly, so it
// needs its own item-atom guard or a `forget --chat <src-id>` is resurrected.
// ---------------------------------------------------------------------------

func TestGovernance_FilesystemChatForgetNoResurrection(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "note.md"), []byte("a private note"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := Source{Name: "docs", Type: "filesystem", Path: srcDir, Scope: "personal"}

	// First walk writes the filesystem memory.
	if n, err := ingestFilesystem(cfg, s, io.Discard); err != nil || n != 1 {
		t.Fatalf("first ingest: n=%d err=%v", n, err)
	}
	id := "src_" + ContentHash(s.Name+":note.md")
	dest := filepath.Join(sourcesRoot(cfg), s.Type, s.Name, id+".md")
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("expected filesystem memory after first ingest: %v", err)
	}

	// Forget it exactly as `mora forget --chat <src-id>` does: record the
	// stable_id suppression, then remove the file.
	if _, err := appendGovernanceEntry(cfg, govEntry{
		Kind: govKindForget, Action: govActionSuppress,
		Atom: govAtom{Kind: atomStableID, Value: id},
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(dest); err != nil {
		t.Fatal(err)
	}

	// The next walk must NOT resurrect it (the bug: ingestFilesystem bypasses the
	// writeMappedMemory guard).
	if n, err := ingestFilesystem(cfg, s, io.Discard); err != nil {
		t.Fatalf("second ingest: %v", err)
	} else if n != 0 {
		t.Fatalf("suppressed filesystem memory must not be re-written, got n=%d", n)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("RESURRECTION: forgotten filesystem memory was re-created by the next walk")
	}
}

// ---------------------------------------------------------------------------
// serialized ledger writes: concurrent forgets must never lose a suppression
// (a dropped entry whose files were removed is a resurrection).
// ---------------------------------------------------------------------------

func TestGovernance_ConcurrentAppendsNoLostUpdate(t *testing.T) {
	cfg := Config{VaultDir: t.TempDir()}
	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := appendGovernanceEntry(cfg, govEntry{
				Kind: govKindForget, Action: govActionSuppress,
				Atom: govAtom{Provider: "imessage", Kind: atomHandle, Value: fmt.Sprintf("+1408555%04d", i)},
			})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent append: %v", err)
		}
	}
	g, err := loadGovernance(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Entries) != n {
		t.Fatalf("lost update: want %d entries, got %d (unlocked read-modify-write clobbered a suppression)", n, len(g.Entries))
	}
}

// ---------------------------------------------------------------------------
// the P13-deferred limitation, made explicit: for Gmail/Calendar the address
// set INCLUDES the user's own address, so a realistic 1:1 thread (self + one
// other = 2 atoms) is NOT sole-matched by `forget --email`. We UNDER-match
// (precision-first) rather than risk deleting a group thread; person-level email
// suppression needs self-identity (P13). See docs/architecture/17.
// ---------------------------------------------------------------------------

func TestGovernance_GmailRealisticOneToOneNotSuppressed(t *testing.T) {
	g := governance{Schema: governanceSchema, Entries: []govEntry{{
		ID: "e1", Kind: govKindForget, Action: govActionSuppress,
		Atom: govAtom{Kind: atomAddress, Value: "sam@example.com"},
	}}}
	// from=[sam], to=[me]: the normal inbound shape, self present ⇒ 2 atoms.
	if sup, _ := g.suppresses(gmailMM("t", []string{"sam@example.com"}, []string{"me@x.com"})); sup {
		t.Fatal("realistic 1:1 email (self included) must NOT be whole-suppressed by --email (P13-deferred)")
	}
}
