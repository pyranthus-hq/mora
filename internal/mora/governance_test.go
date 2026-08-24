package mora

import (
	"context"
	"errors"
	"github.com/pyranthus-hq/mora/internal/atomicio"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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

// ---------------------------------------------------------------------------
// the suppression decision (guard core)
// ---------------------------------------------------------------------------

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

func TestGovernance_ForgetCascadesToDerivedAttachmentAndBlocksReingest(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	dir := t.TempDir()
	pdfPath := filepath.Join(dir, "private.pdf")
	writeMinimalPDF(t, pdfPath, "private attachment evidence")
	parent := imsgMM("attachment-parent", "+14155550123")
	parent.Attachments = []memory.Attachment{{
		Path: pdfPath, MimeType: "application/pdf", Filename: "private.pdf",
	}}
	if n, err := writeAttachmentMemories(cfg, parent); err != nil || n != 1 {
		t.Fatalf("seed attachment: n=%d err=%v", n, err)
	}
	attachmentID := "att_" + memory.ContentHash(parent.StableID+":"+pdfPath)
	attachmentPath := filepath.Join(sourcesRoot(cfg), "imessage", memory.SafeFilename(attachmentID)+".md")
	if _, err := os.Stat(attachmentPath); err != nil {
		t.Fatalf("attachment was not seeded: %v", err)
	}

	run(t, "forget", "--chat", parent.StableID, "--yes")
	if _, err := os.Stat(attachmentPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("parent forget left derived attachment on disk: %v", err)
	}
	if n, err := writeAttachmentMemories(cfg, parent); err != nil || n != 0 {
		t.Fatalf("parent suppression did not block attachment reingest: n=%d err=%v", n, err)
	}
}

func TestGovernance_ForgetCascadesToPreUpgradeAttachmentWithoutParentMeta(t *testing.T) {
	for _, tc := range []struct {
		name     string
		selector []string
	}{
		{name: "stable id", selector: []string{"--chat", "imessage_chat/legacy-parent"}},
		{name: "sole handle", selector: []string{"--handle", "+14155550123"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := coreBIngestInitCfg(t)
			parent := imsgMM("legacy-parent", "+14155550123")
			if err := writeMappedMemory(cfg, parent); err != nil {
				t.Fatal(err)
			}
			legacyPath := filepath.Join(t.TempDir(), "legacy.pdf")
			attachmentID := "att_" + memory.ContentHash(parent.StableID+":"+legacyPath)
			legacy := memory.MappedMemory{
				StableID: attachmentID, Provider: "imessage", ProviderID: "legacy-attachment",
				Type: "source", Title: "legacy.pdf", Body: "pre-upgrade attachment evidence",
				Source: legacyPath, Scope: "personal", CreatedAt: "2026-01-01T00:00:00Z",
				ContentHash: memory.ContentHash("legacy.pdf", "pre-upgrade attachment evidence"),
				Meta:        nil, // exact pre-#115 shape: no parent provenance
			}
			if err := writeMappedMemory(cfg, legacy); err != nil {
				t.Fatal(err)
			}
			parentPath := filepath.Join(sourcesRoot(cfg), "imessage", memory.SafeFilename(parent.StableID)+".md")
			attachmentPath := filepath.Join(sourcesRoot(cfg), "imessage", memory.SafeFilename(attachmentID)+".md")

			args := append([]string{"forget"}, tc.selector...)
			args = append(args, "--yes")
			run(t, args...)
			for _, path := range []string{parentPath, attachmentPath} {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("legacy parent forget left %s on disk: %v", path, err)
				}
			}
		})
	}
}

func TestGovernance_ParentForgetAfterAttachmentPrecheckBlocksDerivedWrite(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	dir := t.TempDir()
	pdfPath := filepath.Join(dir, "race.pdf")
	writeMinimalPDF(t, pdfPath, "attachment race evidence")
	parent := imsgMM("attachment-race", "+14155550123")
	parent.Attachments = []memory.Attachment{{
		Path: pdfPath, MimeType: "application/pdf", Filename: "race.pdf",
	}}
	testHookAttachmentAfterParentCheck = func() {
		testHookAttachmentAfterParentCheck = nil
		run(t, "forget", "--chat", parent.StableID, "--yes")
	}
	t.Cleanup(func() { testHookAttachmentAfterParentCheck = nil })
	if n, err := writeAttachmentMemories(cfg, parent); err != nil || n != 0 {
		t.Fatalf("attachment wrote after parent forget committed: n=%d err=%v", n, err)
	}
	attachmentID := "att_" + memory.ContentHash(parent.StableID+":"+pdfPath)
	attachmentPath := filepath.Join(sourcesRoot(cfg), "imessage", memory.SafeFilename(attachmentID)+".md")
	if _, err := os.Stat(attachmentPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("racing parent forget left derived attachment: %v", err)
	}
}

func TestGovernance_ForgetScanAndAppendAreAtomicAgainstWriter(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	mm := imsgMM("scan-append-race", "+14155550123")
	writerStarted := make(chan struct{})
	writerDone := make(chan error, 1)
	testHookForgetAfterScan = func() {
		if _, err := os.Stat(governanceLockPath(cfg)); err != nil {
			t.Errorf("forget scan hook ran without the governance lease: %v", err)
		}
		go func() {
			close(writerStarted)
			writerDone <- writeMappedMemory(cfg, mm)
		}()
		<-writerStarted
	}
	t.Cleanup(func() { testHookForgetAfterScan = nil })

	run(t, "forget", "--chat", mm.StableID, "--yes")
	if err := <-writerDone; err != nil {
		t.Fatalf("competing writer: %v", err)
	}
	path := filepath.Join(sourcesRoot(cfg), "imessage", memory.SafeFilename(mm.StableID)+".md")
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("scan→append race materialized forgotten content: %v", err)
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
	// Phase 4 skips unchanged files by manifest. Change the provider record so the
	// second incremental walk enters the same suppress-check/write window.
	if err := os.WriteFile(filepath.Join(srcDir, "note.md"), []byte("a private note, updated"), 0o644); err != nil {
		t.Fatal(err)
	}

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

// transientContentionTimeout bounds retryTransientContention. It is generous
// relative to acquireGovernanceLock's own bounded spin (the
// governanceAcquireTimeout wall-clock budget, jittered backoff within it): under
// heavy Windows CI contention that internal spin can
// still be exhausted, at which point the lease returns its fail-fast "retry in a
// moment" error by design. A real caller — and this test, standing in for one —
// must retry that transient error rather than treat it as fatal. Only liveness
// is made robust here; the lease's mutual exclusion (the no-resurrection SAFETY
// guarantee) is untouched. See issue #115.
const transientContentionTimeout = 30 * time.Second

// isTransientContention reports whether err is a retryable contention error:
// the governance/sources lease's fail-fast "retry in a moment" (a plain
// fmt.Errorf with no typed sentinel to errors.Is against), OR — on Windows only
// — an ERROR_SHARING_VIOLATION/ACCESS_DENIED from a file op racing a concurrent
// rename/remove (sharingViolationRetryable is always false off Windows, so this
// clause adds no non-Windows behavior). Anything else is a genuine failure.
func isTransientContention(err error) bool {
	return err != nil &&
		(strings.Contains(err.Error(), "retry in a moment") || atomicio.SharingViolationRetryable(err))
}

// retryTransientContention re-runs fn until it succeeds, returns a non-transient
// error, or the generous deadline elapses — mirroring how a real caller
// (writeMappedMemory during sync, cmdForget) must treat the lease's fail-fast
// contract (issue #115). It NEVER masks a genuine failure: a non-transient error
// is returned immediately, and an op still contended past the deadline returns
// its last contention error, so the test still fails loudly on a real deadlock —
// just not on a transient lock-contention blip. Retrying is safe because each
// wrapped op is all-or-nothing under the lease: a transient acquire failure
// means it did NOT append/write, so a retry performs the effect exactly once.
func retryTransientContention(fn func() error) error {
	deadline := time.Now().Add(transientContentionTimeout)
	for attempt := 0; ; attempt++ {
		err := fn()
		if err == nil || !isTransientContention(err) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(sourcesAcquireBackoff(attempt))
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
				if err := retryTransientContention(func() error { return writeMappedMemory(cfg, mm) }); err != nil {
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
		if err := retryTransientContention(func() error {
			_, err := appendGovernanceEntry(cfg, govEntry{
				Kind: govKindForget, Action: govActionSuppress,
				Atom: govAtom{Provider: "imessage", Kind: atomHandle, Value: "+14155550123"},
			})
			return err
		}); err != nil {
			t.Errorf("forget append: %v", err)
			return
		}
		// The remove is a raw os.Remove (as cmdForget does), NOT the writers'
		// self-retrying atomicWrite rename, so on Windows it can lose a race with a
		// concurrent writer's rename-into-dest and return ERROR_SHARING_VIOLATION.
		// Retry that transient error (still tolerating IsNotExist); the durable-first
		// order is preserved because the suppression append already committed above.
		if err := retryTransientContention(func() error {
			if err := os.Remove(dest); err != nil && !os.IsNotExist(err) {
				return err
			}
			return nil
		}); err != nil {
			t.Errorf("forget remove: %v", err)
		}
	}()

	ready.Wait()
	close(release)
	wg.Wait()

	// The suppression is committed, so a final re-sync pass must be a no-op and the
	// file must stay gone — no writer may have resurrected it.
	if err := retryTransientContention(func() error { return writeMappedMemory(cfg, mm) }); err != nil {
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
