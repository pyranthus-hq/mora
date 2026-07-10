package mora

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
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
// the REAL race the sequential test above cannot catch (#113): a `mora forget`
// that commits its suppression AND removes the file DURING an in-flight
// ingestFilesystem walk — after a once-per-walk ledger snapshot would have been
// loaded but before the walker writes the file. The snapshot approach resurrected
// it; the per-file re-check under the governance lease (writeUnlessForgotten)
// closes the window. Deterministic (no goroutines/sleeps): a seam fires the REAL
// `mora forget --chat` path exactly in that window.
// ---------------------------------------------------------------------------

func TestGovernance_FilesystemForgetDuringWalkNoResurrection(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "note.md"), []byte("a private note"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := Source{Name: "docs", Type: "filesystem", Path: srcDir, Scope: "personal"}

	// First walk materializes the filesystem memory (no seam armed yet).
	if n, err := ingestFilesystem(cfg, s, io.Discard); err != nil || n != 1 {
		t.Fatalf("first ingest: n=%d err=%v", n, err)
	}
	id := "src_" + ContentHash(s.Name+":note.md")
	dest := filepath.Join(sourcesRoot(cfg), s.Type, s.Name, id+".md")
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("expected filesystem memory after first ingest: %v", err)
	}

	// Arm the seam: fire the REAL forget path exactly once, in the window a stale
	// walker would use — right before this file's suppress-check-and-write. The
	// hook runs `mora forget --chat` to completion (suppression committed via the
	// governance lease + file removed) BEFORE the walker's own re-check, faithfully
	// reproducing a forget that commits mid-walk. Synchronous ⇒ deterministic.
	fired := 0
	testHookFSPreWrite = func(gotID string) {
		if gotID != id || fired > 0 {
			return
		}
		fired++
		run(t, "forget", "--chat", id, "--yes")
	}
	t.Cleanup(func() { testHookFSPreWrite = nil })

	// Second walk: with a once-per-walk snapshot this resurrects the memory; the
	// per-file re-check under the lease must honor the mid-walk forget instead.
	n, err := ingestFilesystem(cfg, s, io.Discard)
	if err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	if fired != 1 {
		t.Fatalf("seam did not fire (fired=%d) — the race was not exercised", fired)
	}
	if n != 0 {
		t.Fatalf("walker wrote %d file(s) after a mid-walk forget; want 0 (RESURRECTION)", n)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("RESURRECTION: a forget that committed mid-walk was undone by the stale walker")
	}
	// The forget really ran the durable path: exactly one active suppression.
	g, err := loadGovernance(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.activeSuppress()) != 1 {
		t.Fatalf("mid-walk forget must record 1 active suppression, got %d", len(g.activeSuppress()))
	}
}

// The above test proves the walk re-reads the ledger PER FILE; this one proves it
// does so UNDER THE LEASE — i.e. the governance lock is held ACROSS the write, not
// merely re-loaded. A per-file reload without the lease would still leave #113
// open to a truly concurrent forget (append+remove landing between the walker's
// check and its atomicWrite). Deterministic: the seam fires synchronously inside
// the write critical section and asserts the lease file is present.
func TestGovernance_FilesystemWriteHoldsGovernanceLease(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "note.md"), []byte("a private note"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := Source{Name: "docs", Type: "filesystem", Path: srcDir, Scope: "personal"}

	held := 0
	testHookInWriteCritical = func() {
		held++
		// While we are between the suppression check and the atomicWrite, the
		// governance lease must be held — so its lock file must exist. If a refactor
		// dropped the lock around the write (keeping only the per-file reload), the
		// file would be absent here and #113 would be reopened for a concurrent forget.
		if _, err := os.Stat(governanceLockPath(cfg)); err != nil {
			t.Errorf("governance lease NOT held during the filesystem write (#113): %v", err)
		}
	}
	t.Cleanup(func() { testHookInWriteCritical = nil })

	if n, err := ingestFilesystem(cfg, s, io.Discard); err != nil || n != 1 {
		t.Fatalf("ingest: n=%d err=%v", n, err)
	}
	if held != 1 {
		t.Fatalf("write-critical seam fired %d times, want 1", held)
	}
}

// ---------------------------------------------------------------------------
// serialized ledger writes: concurrent forgets must never lose a suppression
// (a dropped entry whose files were removed is a resurrection).
// ---------------------------------------------------------------------------

func TestGovernance_ConcurrentAppendsNoLostUpdate(t *testing.T) {
	cfg := Config{VaultDir: t.TempDir()}
	const n = 16

	// A start-barrier releases all n appenders at once, and a widened
	// read-modify-write window (below) makes them observe the SAME pre-append
	// ledger — so WITHOUT the lease every save clobbers the others and only one
	// entry survives (a lost update = a resurrected suppression). WITH the lease
	// the n RMWs serialize and every entry survives. The barrier is what makes the
	// failure deterministic: without it, fast sequential appends can each finish
	// before the next starts, so the old test passed even with the lease removed.
	// (This lost update is at the FILESYSTEM level — each goroutine has its own
	// in-memory ledger — so `go test -race` does NOT flag it; the count assertion
	// is the sole detector, which is exactly why the barrier is required.)
	testHookGovAppendPostLoad = func() {
		// Widen the load→save window so overlapping unlocked writers collide. Under
		// the lease this only slows one serialized holder at a time (no overlap).
		for i := 0; i < 500; i++ {
			runtime.Gosched()
		}
	}
	t.Cleanup(func() { testHookGovAppendPostLoad = nil })

	release := make(chan struct{})
	var ready, wg sync.WaitGroup
	ready.Add(n)
	wg.Add(n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			ready.Done() // parked at the barrier
			<-release    // ...released simultaneously with the others
			_, err := appendGovernanceEntry(cfg, govEntry{
				Kind: govKindForget, Action: govActionSuppress,
				Atom: govAtom{Provider: "imessage", Kind: atomHandle, Value: fmt.Sprintf("+1408555%04d", i)},
			})
			errs <- err
		}(i)
	}
	ready.Wait()   // every goroutine is at the barrier
	close(release) // fire them together
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
// #113 gap #1: the CONNECTOR write path (writeMappedMemory) must hold the
// governance lease ACROSS its suppression check and its atomicWrite — the same
// TOCTOU class the filesystem path closed. A fresh once-per-item load alone
// closes only the stale-snapshot variant; a forget committing between an unlocked
// check and the write is still missed and resurrects the atom on re-sync.
// ---------------------------------------------------------------------------

// TestGovernance_MappedWriteHoldsGovernanceLease is the DETERMINISTIC regression
// for the connector check-to-write window (the twin of
// TestGovernance_FilesystemWriteHoldsGovernanceLease). A seam fires inside
// writeMappedMemory, after the suppression check and content-hash skip and while
// the governance lease is held, and asserts the lease FILE is present. Because
// `mora forget`'s suppression append takes that same lease, its being held here
// PROVES no forget can commit between the check and the write — so none can be
// missed and resurrect. Revert the fix (back to a lease-less shouldSuppressWrite +
// atomicWrite) and the lock file is absent here, failing the test.
func TestGovernance_MappedWriteHoldsGovernanceLease(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	held := 0
	testHookMappedWriteCritical = func() {
		held++
		// Between the suppression check and the atomicWrite the governance lease
		// must be held — its lock file must exist. If a refactor dropped the lease
		// around the connector write (keeping only a one-shot check), the file would
		// be absent here and #113 gap #1 would be reopened for a concurrent forget.
		if _, err := os.Stat(governanceLockPath(cfg)); err != nil {
			t.Errorf("governance lease NOT held during the connector write (#113 gap #1): %v", err)
		}
	}
	t.Cleanup(func() { testHookMappedWriteCritical = nil })

	if err := writeMappedMemory(cfg, imsgMM("solo", "+14155550123")); err != nil {
		t.Fatalf("writeMappedMemory: %v", err)
	}
	if held != 1 {
		t.Fatalf("connector write-critical seam fired %d times, want 1", held)
	}
}

// TestGovernance_ConcurrentSyncAndForgetNoResurrection is the behavioral
// companion: a `mora forget` racing an hourly re-sync must never leave the
// forgotten memory resurrected. Many writer goroutines re-persist the live item
// (writeMappedMemory) while one goroutine runs the durable forget (append the
// suppression under the lease, then remove the file — the exact cmdForget order).
// The lease serializes each writer's check-and-write against the append, so the
// forget either commits before a writer's check (the writer skips) or after the
// writer releases (the forget's remove reaps the just-written file). Either way,
// once the suppression is committed the final on-disk state has NO file. This
// passes deterministically WITH the fix; without it, a writer that checked before
// the commit but wrote after the remove resurrects the atom.
func TestGovernance_ConcurrentSyncAndForgetNoResurrection(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	mm := imsgMM("solo", "+14155550123")
	dest := filepath.Join(sourcesRoot(cfg), "imessage", memory.SafeFilename(mm.StableID)+".md")

	// Seed the file (the first sync) so the forget's remove has something to reap.
	if err := writeMappedMemory(cfg, mm); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	const writers = 12
	const iterations = 40
	release := make(chan struct{})
	var ready, wg sync.WaitGroup
	ready.Add(writers + 1)
	wg.Add(writers + 1)

	// Writers: re-persist the live item repeatedly (the hourly re-sync re-fetches
	// it every pass) for the whole race window.
	for w := 0; w < writers; w++ {
		go func() {
			defer wg.Done()
			ready.Done()
			<-release
			for i := 0; i < iterations; i++ {
				if err := writeMappedMemory(cfg, mm); err != nil {
					t.Errorf("re-sync write: %v", err)
					return
				}
			}
		}()
	}
	// Forget: durable-first (append suppression, then remove the file), the exact
	// order cmdForget uses so a crash can never leave files gone but re-ingestable.
	go func() {
		defer wg.Done()
		ready.Done()
		<-release
		if _, err := appendGovernanceEntry(cfg, govEntry{
			Kind: govKindForget, Action: govActionSuppress,
			Atom: govAtom{Provider: "imessage", Kind: atomHandle, Value: "+14155550123"},
		}); err != nil {
			t.Errorf("forget append: %v", err)
			return
		}
		if err := os.Remove(dest); err != nil && !os.IsNotExist(err) {
			t.Errorf("forget remove: %v", err)
		}
	}()

	ready.Wait()
	close(release)
	wg.Wait()

	// The suppression is committed, so a final re-sync pass must be a no-op and the
	// file must stay gone — no writer may have resurrected it.
	if err := writeMappedMemory(cfg, mm); err != nil {
		t.Fatalf("post-forget write: %v", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("RESURRECTION: a forget racing re-sync left the forgotten memory on disk")
	}
	g, err := loadGovernance(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.activeSuppress()) != 1 {
		t.Fatalf("want exactly 1 active suppression, got %d", len(g.activeSuppress()))
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
