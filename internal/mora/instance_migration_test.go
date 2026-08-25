package mora

import (
	"context"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	commitmentpkg "github.com/pyranthus-hq/mora/internal/commitment"
	"github.com/pyranthus-hq/mora/internal/memory"
)

// instance_migration_test.go — issue #495 regression gates. A vault that
// predates per-instance source naming holds UNSUFFIXED connector files; the
// first account-suffixed sync must supersede them (same provider object only),
// and index rebuild must survive any stray twin with a warning instead of a raw
// UNIQUE-constraint crash.

const instanceMigrationNow = "2026-08-24T12:00:00Z"

// seedSourceFile writes a memory file at its connector-source path (the
// sources/<provider>/ tree), bypassing writeMappedMemory — exactly how a
// pre-#475 vault's files (or a restored backup) sit on disk.
func seedSourceFile(t *testing.T, cfg Config, m Memory) string {
	t.Helper()
	path := filepath.Join(sourcesRoot(cfg), m.Provider, memory.SafeFilename(m.ID)+".md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := renderMemory(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicio.Write(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func legacyCalendarEvent(id string, syncedAt string) Memory {
	return Memory{
		ID: id, Scope: "global", Type: "event", Title: "Legacy sync", Source: "calendar",
		Provider: "calendar", ProviderID: "ev1", CreatedAt: "2026-07-01T09:00:00Z",
		LastSynced: syncedAt, Text: "legacy body",
	}
}

func calendarEventMM(account, syncedAt string) memory.MappedMemory {
	stableID := "calendar_event/ev1"
	if account != "" {
		stableID += "@" + account
	}
	return memory.MappedMemory{
		StableID: stableID, Scope: "global", Type: "event", Title: "Fresh sync",
		Body: "fresh body", Source: "calendar", Provider: "calendar", ProviderID: "ev1",
		Account: account, CreatedAt: instanceMigrationNow, ContentHash: "h1", LastSynced: syncedAt,
	}
}

// TestInstanceMigrationSupersedesLegacyTwin — the issue #495 scenario: a vault
// seeded with a pre-#475 unsuffixed non-default-instance file; the first
// account-suffixed sync publishes the canonical twin and retires the legacy
// file. Idempotent: a second sync is a no-op.
func TestInstanceMigrationSupersedesLegacyTwin(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	legacyPath := seedSourceFile(t, cfg, legacyCalendarEvent("calendar_event/ev1", "2026-07-01T09:00:00Z"))
	canonicalPath := filepath.Join(sourcesRoot(cfg), "calendar", "calendar_event_ev1@work.md")

	if _, err := os.Stat(canonicalPath); !os.IsNotExist(err) {
		t.Fatalf("precondition: suffixed canonical must not exist yet (err=%v)", err)
	}
	wrote, err := writeMappedMemoryDetailed(cfg, calendarEventMM("work", instanceMigrationNow))
	if err != nil || !wrote {
		t.Fatalf("writeMappedMemoryDetailed: wrote=%v err=%v", wrote, err)
	}
	if _, err := os.Stat(canonicalPath); err != nil {
		t.Fatalf("canonical suffixed file missing after sync: %v", err)
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("legacy unsuffixed twin was not superseded (err=%v)", err)
	}

	// Idempotent on every sync: re-syncing changes nothing and stays green.
	wrote, err = writeMappedMemoryDetailed(cfg, calendarEventMM("work", instanceMigrationNow))
	if err != nil || wrote {
		t.Fatalf("re-sync: wrote=%v err=%v (content unchanged, want skip)", wrote, err)
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatal("legacy twin came back after an idempotent re-sync")
	}
}

// TestInstanceMigrationNeverRemovesNonTwins locks the safety rails: a file that
// is not provably the SAME provider object stays, and so does one that is
// strictly newer than the record being written.
func TestInstanceMigrationNeverRemovesNonTwins(t *testing.T) {
	cfg := coreBIngestInitCfg(t)

	t.Run("different provider object", func(t *testing.T) {
		other := legacyCalendarEvent("calendar_event/ev1", "2026-07-01T09:00:00Z")
		other.ProviderID = "someone-elses-event"
		path := seedSourceFile(t, cfg, other)
		if _, err := writeMappedMemoryDetailed(cfg, calendarEventMM("work", instanceMigrationNow)); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("non-twin legacy file must never be removed: %v", err)
		}
	})

	t.Run("newer last_synced", func(t *testing.T) {
		fresh := legacyCalendarEvent("calendar_event/ev1", "2026-07-01T09:00:00Z")
		fresh.LastSynced = "2026-08-24T13:00:00Z" // strictly AFTER the incoming record
		path := seedSourceFile(t, cfg, fresh)
		if _, err := writeMappedMemoryDetailed(cfg, calendarEventMM("work", instanceMigrationNow)); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("strictly-newer legacy evidence must be kept: %v", err)
		}
	})

	t.Run("missing provider_id anchor", func(t *testing.T) {
		noAnchor := legacyCalendarEvent("calendar_event/ev1", "2026-07-01T09:00:00Z")
		noAnchor.ProviderID = ""
		path := seedSourceFile(t, cfg, noAnchor)
		if _, err := writeMappedMemoryDetailed(cfg, calendarEventMM("work", instanceMigrationNow)); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("identity-unprovable file must be kept: %v", err)
		}
	})
}

// gmailTwinMemory builds the twin fixture both forms share: provider-identity
// message refs (internal/google/gmail.go gmailMessageRef) are independent of
// the StableID suffix, so BOTH files derive the identical commitment_id — the
// exact UNIQUE(commitment_id) crash from the live reproduction.
func gmailTwinMemory(id, account, title string) Memory {
	m := Memory{
		ID: id, Account: account, Scope: "global", Type: "email", Provider: "gmail",
		ProviderID: "t1", Source: "gmail", Title: title,
		CreatedAt: "2026-08-01T10:00:00Z", LastSynced: instanceMigrationNow,
		Text: "From: Other <other@example.com>\n\nCould you deliver the calibration sheet tomorrow?",
		Meta: map[string]any{
			"from": []string{"other@example.com"},
			"to":   []string{"self@example.com"},
			"messages": []commitmentpkg.GmailMessage{{
				MessageRef: "gmail_thread/t1#m1",
				Sender:     "other@example.com",
				To:         []string{"self@example.com"},
				At:         "2026-08-01T10:00:00Z",
				BlockRefs:  []string{"authored-body", "footer"},
			}},
		},
	}
	return m
}

// captureStderr redirects os.Stderr for the duration of fn.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = saved }()
	fn()
	w.Close()
	out, _ := io.ReadAll(r)
	return string(out)
}

// countCommitmentRows returns (rows with non-empty commitment_id, rows bound to
// the suffixed memory).
func countCommitmentRows(t *testing.T, cfg Config) (int, int) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var total, suffixed int
	if err := db.QueryRow(`SELECT COUNT(*) FROM commitments WHERE commitment_id != ''`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM commitments WHERE commitment_id != '' AND memory_id = 'gmail_thread/t1@work'`).Scan(&suffixed); err != nil {
		t.Fatal(err)
	}
	return total, suffixed
}

// TestIndexRebuildSurvivesStrayInstanceTwin — defense in depth. Even when a
// stray unsuffixed twin remains in the vault (backup restore, out-of-band
// edit), rebuild must succeed, keep ONLY the suffixed side's row, and warn with
// both paths instead of dying on UNIQUE(commitment_id).
func TestIndexRebuildSurvivesStrayInstanceTwin(t *testing.T) {
	t.Setenv("MORA_EMBEDDER", "") // deterministic static embedder
	cfg := sandboxCfg(t)
	cfg.SelfEmails = []string{"self@example.com"} // resolve the fixture's counterparty deterministically

	// The post-#475 sync writes the suffixed canonical form.
	mm := memory.MappedMemory{
		StableID: "gmail_thread/t1@work", Account: "work", Scope: "global", Type: "email",
		Title: "Calibration sheet", Body: "Could you deliver the calibration sheet tomorrow?",
		Source: "gmail", Provider: "gmail", ProviderID: "t1",
		CreatedAt: "2026-08-01T10:00:00Z", ContentHash: "h1", LastSynced: instanceMigrationNow,
		Meta: gmailTwinMemory("", "", "").Meta,
	}
	if err := writeMappedMemory(cfg, mm); err != nil {
		t.Fatal(err)
	}
	// A backup restore drops the pre-#475 unsuffixed twin back into the vault.
	legacyPath := seedSourceFile(t, cfg, gmailTwinMemory("gmail_thread/t1", "", "Calibration sheet"))

	if _, err := createVaultMarkerIfAbsent(cfg, "v_twin"); err != nil {
		t.Fatal(err)
	}
	var count int
	var rebuildErr error
	stderr := captureStderr(t, func() {
		count, rebuildErr = rebuildIndex(context.Background(), cfg)
	})
	if rebuildErr != nil {
		t.Fatalf("rebuild must survive a stray instance twin; got: %v", rebuildErr)
	}
	if count < 1 {
		t.Fatalf("rebuild indexed %d memories, want >= 1", count)
	}
	total, suffixed := countCommitmentRows(t, cfg)
	if total != 1 || suffixed != 1 {
		t.Fatalf("commitments rows total=%d suffixed=%d, want exactly the canonical twin's single row", total, suffixed)
	}
	for _, want := range []string{legacyPath} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("twin warning must name %s; stderr:\n%s", want, stderr)
		}
	}
}

// TestCanonicalInstanceTwinRejectsLabeledUnsuffixed — an unsuffixed memory
// that carries its OWN account label is a distinct instance (provider ids can
// be account-local), never legacy residue: rebuild dedup must not merge it.
func TestCanonicalInstanceTwinRejectsLabeledUnsuffixed(t *testing.T) {
	labeled := Memory{ID: "gmail_thread/t1", Account: "home", Provider: "gmail", ProviderID: "t1"}
	suffixed := Memory{ID: "gmail_thread/t1@work", Account: "work", Provider: "gmail", ProviderID: "t1"}
	if _, _, ok := canonicalInstanceTwin(labeled, suffixed); ok {
		t.Fatal("account-labeled unsuffixed memory must not be treated as a legacy twin")
	}
	if _, _, ok := canonicalInstanceTwin(suffixed, labeled); ok {
		t.Fatal("twin detection must reject the labeled-unsuffixed pair in either order")
	}
	legacy := Memory{ID: "gmail_thread/t1", Provider: "gmail", ProviderID: "t1"}
	keep, drop, ok := canonicalInstanceTwin(legacy, suffixed)
	if !ok || keep.ID != suffixed.ID || drop.ID != legacy.ID {
		t.Fatalf("true legacy twin must still resolve suffixed-wins: keep=%q drop=%q ok=%v", keep.ID, drop.ID, ok)
	}
}

// TestInstanceMigrationLeavesLegacyWhenCanonicalIsSymlink — a canonical path
// that is a symlink (possibly aliasing the legacy file itself) must never
// authorize removing the legacy file.
func TestInstanceMigrationLeavesLegacyWhenCanonicalIsSymlink(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	legacyPath := seedSourceFile(t, cfg, legacyCalendarEvent("calendar_event/ev1", "2026-07-01T09:00:00Z"))
	canonicalPath := filepath.Join(sourcesRoot(cfg), "calendar", "calendar_event_ev1@work.md")
	if err := os.Symlink(legacyPath, canonicalPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	migrateLegacyInstanceFile(cfg, calendarEventMM("work", instanceMigrationNow))
	if _, err := os.Stat(legacyPath); err != nil {
		t.Fatalf("legacy file must survive when the canonical is a symlink: %v", err)
	}
}

// TestInsertCommitmentRowsRefusesNonTwinDuplicate — two memories that are NOT
// twins of one provider object but derive the same commitment_id are genuinely
// distinct commitments: that stays a hard error naming both, never a silent merge.
func TestInsertCommitmentRowsRefusesNonTwinDuplicate(t *testing.T) {
	memA := gmailTwinMemory("gmail_thread/t1", "", "One")
	memB := gmailTwinMemory("gmail_thread/t9", "", "Two") // different provider object entirely
	memA.Path, memB.Path = "/vault/a.md", "/vault/b.md"
	byID := map[string]Memory{memA.ID: memA, memB.ID: memB}
	cid := commitmentpkg.ID("gmail_thread/t1#m1", "authored-body", 0)
	commitments := []Commitment{
		{ID: cid, OpenedBy: commitmentpkg.Span{MemoryID: memA.ID}},
		{ID: cid, OpenedBy: commitmentpkg.Span{MemoryID: memB.ID}},
	}
	cfg := sandboxCfg(t)
	db, err := sql.Open("sqlite", rwIndexDSN(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS commitments (
		generation TEXT NOT NULL, row_key TEXT PRIMARY KEY, commitment_id TEXT UNIQUE,
		memory_id TEXT NOT NULL, payload TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	err = insertCommitmentRows(context.Background(), tx, "gen", commitments, byID)
	tx.Rollback() //nolint:errcheck
	if err == nil {
		t.Fatal("distinct-provider duplicate commitment_id must stay a hard error")
	}
	for _, want := range []string{"/vault/a.md", "/vault/b.md", cid} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error must name both paths and the id (%s); got: %v", want, err)
		}
	}
}
