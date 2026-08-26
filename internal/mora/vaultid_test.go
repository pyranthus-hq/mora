package mora

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	configstore "github.com/pyranthus-hq/mora/internal/config"
)

// pinOperationClockForTest now lives in testctx_test.go (per-test env, not the
// former operationClock package global).

func sandboxCfg(t *testing.T) Config {
	t.Helper()
	// Same isolation the MORA_CONFIG_DIR Setenv used to produce (root/vault,
	// root/data, root/state, config.toml in the root), but carried through the
	// test's context instead of process state, so sandboxed tests can run in
	// parallel alongside others resolving their own roots.
	env := &testEnv{configRoot: t.TempDir()}
	bindTestEnv(t, env)
	pinOperationClockForTest(t, time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC))
	cfg, err := configstore.LoadFrom(env.ctx())
	if err != nil {
		t.Fatalf("sandboxCfg: %v", err)
	}
	for _, d := range []string{cfg.VaultDir, cfg.DataDir, cfg.StateDir, cfg.ConfigDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return cfg
}

// TestCorruptMarkerFailsLoud covers F2: a corrupt marker must fail loud (it is
// identity-critical), not be silently treated as absent — which would disable
// the rebuild guard exactly when the vault's identity is in doubt.
func TestCorruptMarkerFailsLoud(t *testing.T) {
	cfg := sandboxCfg(t)
	if err := os.WriteFile(markerPath(cfg), []byte("{bad json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeMemory(cfg, Memory{ID: newID(), Scope: "global", Type: "insight", Title: "x", Source: "manual", CreatedAt: nowRFC3339(), Text: "y"}); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("rebuild should surface the corrupt-marker error; got: %v", err)
	}
}

func TestIndexMetaRoundTrip(t *testing.T) {
	cfg := sandboxCfg(t)
	// Seed one memory so the rebuild has content and binds an id.
	if err := writeMemory(cfg, Memory{ID: newID(), Scope: "global", Type: "insight", Title: "x", Source: "manual", CreatedAt: nowRFC3339(), Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	if _, err := createVaultMarkerIfAbsent(cfg, "v_seed"); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	id, err := readIndexVaultID(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if id != "v_seed" {
		t.Fatalf("index vault_id = %q, want v_seed", id)
	}
}

func indexCount(t *testing.T, cfg Config) int {
	t.Helper()
	db, err := sql.Open("sqlite", roIndexDSN(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRebuildEnforceBlocksEmptyVault(t *testing.T) {
	cfg := sandboxCfg(t)
	id := newID()
	if err := writeMemory(cfg, Memory{ID: id, Scope: "global", Type: "insight", Title: "keep", Source: "manual", CreatedAt: nowRFC3339(), Text: "precious"}); err != nil {
		t.Fatal(err)
	}
	if _, err := createVaultMarkerIfAbsent(cfg, "v_live"); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if got := indexCount(t, cfg); got != 1 {
		t.Fatalf("seed index count = %d, want 1", got)
	}

	// Simulate the incident: the vault's memory files vanish (dir emptied).
	if err := os.RemoveAll(memoriesRoot(cfg)); err != nil {
		t.Fatal(err)
	}

	_, err := rebuildIndex(context.Background(), cfg)
	if !errors.Is(err, errRebuildBlocked) {
		t.Fatalf("expected errRebuildBlocked, got %v", err)
	}
	// CRITICAL: the old index must be intact (rolled back, not wiped).
	if got := indexCount(t, cfg); got != 1 {
		t.Fatalf("index count after blocked rebuild = %d, want 1 (preserved)", got)
	}
}

// TestAutoHealBlocksEmptyVault proves the auto-heal path (openIndexRO rebuilding
// a version-stale index inline) runs under Enforce and therefore CANNOT
// resurrect the vault-emptied incident: a stale-schema index over an empty vault
// must surface errRebuildBlocked and leave the old index untouched, not silently
// rebuild to zero.
func TestAutoHealBlocksEmptyVault(t *testing.T) {
	cfg := sandboxCfg(t)
	if err := writeMemory(cfg, Memory{ID: newID(), Scope: "global", Type: "insight", Title: "keep", Source: "manual", CreatedAt: nowRFC3339(), Text: "precious"}); err != nil {
		t.Fatal(err)
	}
	if _, err := createVaultMarkerIfAbsent(cfg, "v_live"); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if got := indexCount(t, cfg); got != 1 {
		t.Fatalf("seed index count = %d, want 1", got)
	}

	// Force the schema stale OUTSIDE any rebuild tx so the next RO open auto-heals.
	wdb, err := sql.Open("sqlite", dbPath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wdb.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatal(err)
	}
	if err := wdb.Close(); err != nil {
		t.Fatal(err)
	}

	// Empty the vault (the incident).
	if err := os.RemoveAll(memoriesRoot(cfg)); err != nil {
		t.Fatal(err)
	}

	_, err = openIndexRO(context.Background(), cfg)
	if !errors.Is(err, errRebuildBlocked) {
		t.Fatalf("auto-heal over an empty vault must block, got %v", err)
	}
	// The auto-heal must NOT have silently rebuilt: schema still stale, corpus intact.
	rdb, err := sql.Open("sqlite", roIndexDSN(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer rdb.Close()
	var v int
	if err := rdb.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != 1 {
		t.Fatalf("user_version = %d, want 1 (auto-heal must not have rebuilt)", v)
	}
	if got := indexCount(t, cfg); got != 1 {
		t.Fatalf("index count after blocked auto-heal = %d, want 1 (preserved)", got)
	}
}

func TestRebuildAllowCommitsEmpty(t *testing.T) {
	cfg := sandboxCfg(t)
	if err := writeMemory(cfg, Memory{ID: newID(), Scope: "global", Type: "insight", Title: "x", Source: "manual", CreatedAt: nowRFC3339(), Text: "y"}); err != nil {
		t.Fatal(err)
	}
	if _, err := createVaultMarkerIfAbsent(cfg, "v_live"); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(memoriesRoot(cfg)); err != nil {
		t.Fatal(err)
	}
	n, err := rebuildIndexWithPolicy(context.Background(), cfg, policyAllow)
	if err != nil {
		t.Fatalf("allow policy should commit, got %v", err)
	}
	if n != 0 || indexCount(t, cfg) != 0 {
		t.Fatalf("allow rebuild count = %d, index = %d, want 0/0", n, indexCount(t, cfg))
	}
}

func TestBlockRecordWrittenAndCleared(t *testing.T) {
	cfg := sandboxCfg(t)
	id := newID()
	if err := writeMemory(cfg, Memory{ID: id, Scope: "global", Type: "insight", Title: "k", Source: "manual", CreatedAt: nowRFC3339(), Text: "v"}); err != nil {
		t.Fatal(err)
	}
	if _, err := createVaultMarkerIfAbsent(cfg, "v_live"); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(memoriesRoot(cfg)); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); !errors.Is(err, errRebuildBlocked) {
		t.Fatalf("want blocked, got %v", err)
	}
	if _, present, _ := readBlockRecord(cfg); !present {
		t.Fatal("block record should exist after a blocked rebuild")
	}
	// Restore the vault; a good rebuild must clear the record.
	if err := writeMemory(cfg, Memory{ID: id, Scope: "global", Type: "insight", Title: "k", Source: "manual", CreatedAt: nowRFC3339(), Text: "v"}); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if _, present, _ := readBlockRecord(cfg); present {
		t.Fatal("block record should be cleared after a successful rebuild")
	}
}

func TestRebuildBlocksForeignVault(t *testing.T) {
	cfg := sandboxCfg(t)
	id := newID()
	if err := writeMemory(cfg, Memory{ID: id, Scope: "global", Type: "insight", Title: "k", Source: "manual", CreatedAt: nowRFC3339(), Text: "v"}); err != nil {
		t.Fatal(err)
	}
	if _, err := createVaultMarkerIfAbsent(cfg, "v_a"); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if got := indexCount(t, cfg); got != 1 {
		t.Fatalf("seed index count = %d, want 1", got)
	}
	// Overwrite the vault marker with a different id to simulate a foreign vault.
	m := vaultMarker{Schema: vaultMarkerSchema, VaultID: "v_b", CreatedAt: nowRFC3339(), CreatedBy: "test"}
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(markerPath(cfg), body, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); !errors.Is(err, errRebuildBlocked) {
		t.Fatalf("want errRebuildBlocked for foreign vault, got %v", err)
	}
	// Old corpus must be preserved.
	if got := indexCount(t, cfg); got != 1 {
		t.Fatalf("index count after blocked foreign-vault rebuild = %d, want 1 (preserved)", got)
	}
}

// TestForceRebuildRecreatesLostMarker covers F1: if the marker file is lost but
// the index still knows its vault id, a rebuild must re-stamp the marker bound to
// that id — otherwise the next Enforce rebuild blocks forever and --force is
// non-idempotent.
func TestForceRebuildRecreatesLostMarker(t *testing.T) {
	cfg := sandboxCfg(t)
	if err := writeMemory(cfg, Memory{ID: newID(), Scope: "global", Type: "insight", Title: "k", Source: "manual", CreatedAt: nowRFC3339(), Text: "v"}); err != nil {
		t.Fatal(err)
	}
	if _, err := createVaultMarkerIfAbsent(cfg, "v_a"); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}

	// Lose the marker (e.g. a botched sync / accidental delete).
	if err := os.Remove(markerPath(cfg)); err != nil {
		t.Fatal(err)
	}

	// A rebuild must re-stamp the marker bound to the index's own id.
	if _, err := rebuildIndexWithPolicy(context.Background(), cfg, policyAllow); err != nil {
		t.Fatalf("allow rebuild with a lost marker should succeed: %v", err)
	}
	m, present, err := readVaultMarker(cfg)
	if err != nil || !present {
		t.Fatalf("marker not recreated: present=%v err=%v", present, err)
	}
	if m.VaultID != "v_a" {
		t.Fatalf("recreated marker id = %q, want v_a (== index_meta.vault_id)", m.VaultID)
	}
	id, err := readIndexVaultID(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if id != "v_a" {
		t.Fatalf("index vault_id = %q, want v_a", id)
	}

	// A subsequent PLAIN Enforce rebuild must now succeed (identity self-healed).
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatalf("Enforce rebuild after self-heal must not block: %v", err)
	}
}

func TestIndexRebuildForceOverridesBlock(t *testing.T) {
	cfg := sandboxCfg(t)
	if err := writeMemory(cfg, Memory{ID: newID(), Scope: "global", Type: "insight", Title: "k", Source: "manual", CreatedAt: nowRFC3339(), Text: "v"}); err != nil {
		t.Fatal(err)
	}
	if _, err := createVaultMarkerIfAbsent(cfg, "v_live"); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(memoriesRoot(cfg)); err != nil {
		t.Fatal(err)
	}
	// Without --force: blocked.
	var out bytes.Buffer
	if err := cmdIndex(testCtx(t), []string{"rebuild"}, &out, testStderr, bytes.NewReader(nil)); !errors.Is(err, errRebuildBlocked) {
		t.Fatalf("want blocked without --force, got %v", err)
	}
	// With --force: succeeds, index empties.
	out.Reset()
	if err := cmdIndex(testCtx(t), []string{"rebuild", "--force"}, &out, testStderr, bytes.NewReader(nil)); err != nil {
		t.Fatalf("force rebuild failed: %v", err)
	}
	if indexCount(t, cfg) != 0 {
		t.Fatalf("force rebuild did not empty the index")
	}
}

func TestInitCreatesMarkerAndPrintsSummary(t *testing.T) {
	cfg := sandboxCfg(t)
	var out bytes.Buffer
	// non-TTY stdin (bytes.Reader) -> setup menu is skipped, summary still prints.
	if err := cmdInit(testCtx(t), nil, &out, testStderr, bytes.NewReader(nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(markerPath(cfg)); err != nil {
		t.Fatalf("init did not create the vault marker: %v", err)
	}
	s := out.String()
	for _, want := range []string{cfg.VaultDir, "Next", "mora connectors"} {
		if !strings.Contains(s, want) {
			t.Fatalf("init summary missing %q; got:\n%s", want, s)
		}
	}
}

// TestInitRepointDiscardsStaleIndex exercises the B3 mechanic: after a confirmed
// `init --vault <new>`, config+marker point at NEW but data_dir still holds the
// OLD vault's index — a hard-wired Enforce rebuild self-blocks. The fix discards
// the stale index so the rebuild is a clean first-build. confirmVaultRepoint
// refuses non-interactively (TTY gate), so this drives the same post-confirm
// state directly rather than through cmdInit's TTY path, and proves both
// branches: WITHOUT the discard the rebuild blocks; WITH it, it proceeds.
func TestInitRepointDiscardsStaleIndex(t *testing.T) {
	cfg := sandboxCfg(t) // vault A = cfg.VaultDir, shared data_dir
	for i := 0; i < 2; i++ {
		if err := writeMemory(cfg, Memory{ID: newID(), Scope: "global", Type: "insight", Title: "a", Source: "manual", CreatedAt: nowRFC3339(), Text: "from vault A"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := createVaultMarkerIfAbsent(cfg, "v_a"); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if got := indexCount(t, cfg); got != 2 {
		t.Fatalf("vault A index count = %d, want 2", got)
	}

	// Repoint to a NEW empty vault B (data_dir unchanged — still holds A's index).
	bDir := t.TempDir()
	cfgB := cfg
	cfgB.VaultDir = bDir
	for _, d := range []string{cfgB.VaultDir, memoriesRoot(cfgB)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := createVaultMarkerIfAbsent(cfgB, "v_b"); err != nil {
		t.Fatal(err)
	}

	// WITHOUT discarding the stale index: an Enforce rebuild self-blocks
	// (oldCount=2 from A's index, newCount=0 from empty B → decBlockEmpty).
	if _, err := rebuildIndex(context.Background(), cfgB); !errors.Is(err, errRebuildBlocked) {
		t.Fatalf("expected the stale-index rebuild to self-block, got %v", err)
	}

	// WITH the discard (what cmdInit now does on a confirmed repoint): remove the
	// index files so the rebuild is a clean first-build for B.
	for _, p := range []string{dbPath(cfgB), dbPath(cfgB) + "-wal", dbPath(cfgB) + "-shm"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	if _, err := rebuildIndex(context.Background(), cfgB); err != nil {
		t.Fatalf("rebuild after discarding the stale index must succeed, got %v", err)
	}
	if got := indexCount(t, cfgB); got != 0 {
		t.Fatalf("fresh vault B index count = %d, want 0", got)
	}
	id, err := readIndexVaultID(context.Background(), cfgB)
	if err != nil {
		t.Fatal(err)
	}
	if id != "v_b" {
		t.Fatalf("repointed index adopted vault_id %q, want v_b", id)
	}
}

// TestConcurrentRebuildsDoNotCorrupt covers B4: a scheduled rebuild (cron) and a
// write-triggered rebuild (MCP) can fire at the same time against one index. With
// the _txlock=immediate DSN each rebuild takes the write lock at BeginTx and a
// contender waits out the busy_timeout instead of deadlocking mid-transaction, so
// every concurrent rebuild commits the full corpus and none returns SQLITE_BUSY.
// (Under the old deferred-lock DSN this races: both begin, one is locked out after
// it already holds a read lock, and cannot retry inside the open tx.)
func TestConcurrentRebuildsDoNotCorrupt(t *testing.T) {
	cfg := sandboxCfg(t)
	const memCount = 25
	for i := 0; i < memCount; i++ {
		if err := writeMemory(cfg, Memory{ID: newID(), Scope: "global", Type: "insight", Title: "m", Source: "manual", CreatedAt: nowRFC3339(), Text: "concurrent corpus"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := createVaultMarkerIfAbsent(cfg, "v_live"); err != nil {
		t.Fatal(err)
	}
	// Prime the index once so every concurrent rebuild is a same-vault re-index
	// (oldCount>0, ids match -> decProceed) — the contended path the cron/MCP race hits.
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}

	const goroutines = 8
	var start sync.WaitGroup
	start.Add(1)
	var done sync.WaitGroup
	errs := make([]error, goroutines)
	for g := 0; g < goroutines; g++ {
		done.Add(1)
		go func(idx int) {
			defer done.Done()
			start.Wait() // release all goroutines together to maximize contention
			_, errs[idx] = rebuildIndex(context.Background(), cfg)
		}(g)
	}
	start.Done()
	done.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent rebuild %d returned an error (want none): %v", i, err)
		}
	}
	// The index must hold the full corpus — not a half-written or empty result.
	if got := indexCount(t, cfg); got != memCount {
		t.Fatalf("index count after %d concurrent rebuilds = %d, want %d", goroutines, got, memCount)
	}
	// The vault-id binding must survive the race intact.
	id, err := readIndexVaultID(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if id != "v_live" {
		t.Fatalf("vault_id after concurrent rebuilds = %q, want v_live", id)
	}
}

// TestInitRepointConfirmedEndToEnd drives the FULL `mora init --vault <new>` repoint
// through the confirmation gate (overridden so no TTY is needed), the path a real
// user hits when relocating their vault. It proves config.toml is rewritten to the
// new vault, the stale old-vault index is discarded, and the rebuild is a clean
// first-build that adopts the NEW vault's identity — never self-blocking on the old
// index's count, and never leaving config=NEW / index=OLD.
func TestInitRepointConfirmedEndToEnd(t *testing.T) {
	cfg := sandboxCfg(t)
	// Establish vault A as a real install: init writes config.toml + marker, then
	// seed two memories and bind the index to A.
	var initOut bytes.Buffer
	if err := cmdInit(testCtx(t), nil, &initOut, testStderr, bytes.NewReader(nil)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := writeMemory(cfg, Memory{ID: newID(), Scope: "global", Type: "insight", Title: "a", Source: "manual", CreatedAt: nowRFC3339(), Text: "from vault A"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if got := indexCount(t, cfg); got != 2 {
		t.Fatalf("vault A index count = %d, want 2", got)
	}
	aVaultID, err := readIndexVaultID(context.Background(), cfg)
	if err != nil || aVaultID == "" {
		t.Fatalf("vault A id: %q err=%v", aVaultID, err)
	}

	// Simulate the user choosing "Repoint" at the TTY confirm.
	orig := confirmVaultRepointFn
	confirmVaultRepointFn = func(io.Reader, io.Writer, string, string) error { return nil }
	t.Cleanup(func() { confirmVaultRepointFn = orig })

	bDir := t.TempDir() // brand-new empty vault location
	var out bytes.Buffer
	if err := cmdInit(testCtx(t), []string{"--vault", bDir}, &out, testStderr, bytes.NewReader(nil)); err != nil {
		t.Fatalf("confirmed repoint must succeed end-to-end, got %v", err)
	}

	// config.toml now points at B (the repoint persisted).
	reloaded, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Clean(reloaded.VaultDir) != filepath.Clean(bDir) {
		t.Fatalf("config vault_dir = %q, want %q (repoint not persisted)", reloaded.VaultDir, bDir)
	}
	// Fresh first-build for empty B: index emptied, not self-blocked at A's count.
	if got := indexCount(t, reloaded); got != 0 {
		t.Fatalf("repointed index count = %d, want 0 (clean first-build for B)", got)
	}
	// The index adopted B's NEW identity, not A's.
	bMarker, present, err := readVaultMarker(reloaded)
	if err != nil || !present {
		t.Fatalf("B marker: present=%v err=%v", present, err)
	}
	bIndexID, err := readIndexVaultID(context.Background(), reloaded)
	if err != nil {
		t.Fatal(err)
	}
	if bIndexID != bMarker.VaultID {
		t.Fatalf("repointed index vault_id = %q, want B's marker id %q", bIndexID, bMarker.VaultID)
	}
	if bIndexID == aVaultID {
		t.Fatalf("repointed index still bound to vault A id %q — identity did not move", aVaultID)
	}
	// Vault A's memory files must remain on disk — a repoint never deletes the old vault.
	aFiles, _ := allMemoryFiles(cfg)
	if len(aFiles) != 2 {
		t.Fatalf("vault A files after repoint = %d, want 2 (old vault must be preserved)", len(aFiles))
	}
}

// TestInitRepointDeclinedKeepsVault verifies the abort path: declining the repoint
// confirmation returns an error and leaves config.toml pointing at the original
// vault, mutating nothing (the confirm gate runs before any write).
func TestInitRepointDeclinedKeepsVault(t *testing.T) {
	cfg := sandboxCfg(t)
	var initOut bytes.Buffer
	if err := cmdInit(testCtx(t), nil, &initOut, testStderr, bytes.NewReader(nil)); err != nil {
		t.Fatal(err)
	}
	aVault := cfg.VaultDir

	orig := confirmVaultRepointFn
	confirmVaultRepointFn = func(io.Reader, io.Writer, string, string) error {
		return errors.New("init cancelled — vault unchanged")
	}
	t.Cleanup(func() { confirmVaultRepointFn = orig })

	bDir := t.TempDir()
	var out bytes.Buffer
	if err := cmdInit(testCtx(t), []string{"--vault", bDir}, &out, testStderr, bytes.NewReader(nil)); err == nil {
		t.Fatal("declined repoint must return an error")
	}
	reloaded, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Clean(reloaded.VaultDir) != filepath.Clean(aVault) {
		t.Fatalf("config vault_dir = %q, want unchanged %q after a declined repoint", reloaded.VaultDir, aVault)
	}
}

// TestDeleteLastMemoryNotBlocked covers F4: deleting the LAST memory empties the
// vault, but delete is the Allow path — it must drop the row from the index, not
// block (which would keep serving the deleted content).
func TestDeleteLastMemoryNotBlocked(t *testing.T) {
	cfg := sandboxCfg(t)
	id := newID()
	if err := writeMemory(cfg, Memory{ID: id, Scope: "global", Type: "insight", Title: "only", Source: "manual", CreatedAt: nowRFC3339(), Text: "delete me"}); err != nil {
		t.Fatal(err)
	}
	if _, err := createVaultMarkerIfAbsent(cfg, "v_live"); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if got := indexCount(t, cfg); got != 1 {
		t.Fatalf("seed index count = %d, want 1", got)
	}
	var out bytes.Buffer
	if err := cmdDelete(testCtx(t), []string{"--yes", id}, &out, testStderr); err != nil {
		t.Fatalf("deleting the last memory must not error: %v", err)
	}
	if got := indexCount(t, cfg); got != 0 {
		t.Fatalf("index count after last-delete = %d, want 0 (deleted content must not stay searchable)", got)
	}
}

// TestDeleteMemoryLastViaAllow covers B1: the MCP delete_memory handler must
// rebuild in Allow mode, so deleting the last memory drops it from the index
// instead of self-blocking (which would keep serving the deleted content).
func TestDeleteMemoryLastViaAllow(t *testing.T) {
	cfg := sandboxCfg(t)
	id := newID()
	if err := writeMemory(cfg, Memory{ID: id, Scope: "global", Type: "insight", Title: "only", Source: "manual", CreatedAt: nowRFC3339(), Text: "delete me"}); err != nil {
		t.Fatal(err)
	}
	if _, err := createVaultMarkerIfAbsent(cfg, "v_live"); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := callMCPTool(testCtx(t), "delete_memory", map[string]any{"id": id}); err != nil {
		t.Fatalf("MCP delete_memory of the last memory must not error: %v", err)
	}
	if got := indexCount(t, cfg); got != 0 {
		t.Fatalf("index count after MCP last-delete = %d, want 0 (deleted content must not stay searchable)", got)
	}
}

// TestDoctorShowsBlockRecord covers F4: after a blocked rebuild, `mora doctor`
// must surface the block in both text and JSON modes.
func TestDoctorShowsBlockRecord(t *testing.T) {
	cfg := sandboxCfg(t)
	if err := writeMemory(cfg, Memory{ID: newID(), Scope: "global", Type: "insight", Title: "k", Source: "manual", CreatedAt: nowRFC3339(), Text: "v"}); err != nil {
		t.Fatal(err)
	}
	if _, err := createVaultMarkerIfAbsent(cfg, "v_live"); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(memoriesRoot(cfg)); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); !errors.Is(err, errRebuildBlocked) {
		t.Fatalf("want a blocked rebuild, got %v", err)
	}

	var txt bytes.Buffer
	if err := cmdDoctor(testCtx(t), nil, &txt, testStderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(txt.String(), "BLOCKED") {
		t.Fatalf("doctor text output should mention BLOCKED; got:\n%s", txt.String())
	}

	var js bytes.Buffer
	if err := cmdDoctor(testCtx(t), []string{"--json"}, &js, testStderr); err != nil {
		t.Fatal(err)
	}
	var rep doctorReport
	if err := json.Unmarshal(js.Bytes(), &rep); err != nil {
		t.Fatalf("doctor --json is not valid JSON: %v\n%s", err, js.String())
	}
	if rep.RebuildBlock == nil {
		t.Fatal("doctor --json should include a non-nil rebuild_block")
	}
	if rep.RebuildBlock.Reason == "" {
		t.Fatal("rebuild_block.reason should be non-empty")
	}
}

func TestConfigShowsPathAnnotations(t *testing.T) {
	cfg := sandboxCfg(t)
	var out bytes.Buffer
	if err := cmdConfig(testCtx(t), nil, &out, testStderr); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{"back this up", "rebuildable", "settings"} {
		if !strings.Contains(s, want) {
			t.Fatalf("config output missing annotation %q; got:\n%s", want, s)
		}
	}
	if !strings.Contains(s, cfg.VaultDir) {
		t.Fatalf("config output missing the vault dir %q; got:\n%s", cfg.VaultDir, s)
	}
}

// seedForeignMarker seeds one memory + marker v_a, rebuilds (binds the index to
// v_a), then overwrites the marker on disk with a different id (v_b) — the exact
// foreign-vault state that makes the next Enforce rebuild block.
func seedForeignMarker(t *testing.T, cfg Config) {
	t.Helper()
	if err := writeMemory(cfg, Memory{ID: newID(), Scope: "global", Type: "insight", Title: "keep", Source: "manual", CreatedAt: nowRFC3339(), Text: "precious"}); err != nil {
		t.Fatal(err)
	}
	if _, err := createVaultMarkerIfAbsent(cfg, "v_a"); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	// Overwrite the marker directly (the marker is write-once via the helper, so
	// a test simulating a foreign vault writes the JSON itself).
	m := vaultMarker{Schema: vaultMarkerSchema, VaultID: "v_b", CreatedAt: nowRFC3339(), CreatedBy: "test"}
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(markerPath(cfg), body, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWriteDegradesWhenRebuildBlocked(t *testing.T) {
	cfg := sandboxCfg(t)
	seedForeignMarker(t, cfg)

	var out, errOut bytes.Buffer
	err := cmdWrite(testCtx(t), []string{"--title", "new note", "--text", "do not lose me"}, &out, &errOut)
	if err != nil {
		t.Fatalf("cmdWrite must NOT fail when the rebuild is blocked (the memory is saved): %v", err)
	}
	// The new memory file must exist on disk (the write is never lost).
	items, lerr := listMemories(cfg, "", 100)
	if lerr != nil {
		t.Fatal(lerr)
	}
	found := false
	for _, m := range items {
		if m.Title == "new note" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the new memory was not persisted to the vault; listMemories=%+v", items)
	}
	// The advisory is a diagnostic, not a result: Plan 01-03 moved it to stderr
	// so the stdout stream stays parseable.
	if !strings.Contains(errOut.String(), "warning") || !strings.Contains(errOut.String(), "index") {
		t.Fatalf("expected a degraded-success warning about the index on stderr; got:\n%s", errOut.String())
	}
}
