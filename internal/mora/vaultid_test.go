package mora

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sandboxCfg(t *testing.T) Config {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("MORA_CONFIG_DIR", dir)
	cfg := defaultConfig()
	for _, d := range []string{cfg.VaultDir, cfg.DataDir, cfg.StateDir, cfg.ConfigDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return cfg
}

func TestVaultMarkerWriteOnce(t *testing.T) {
	cfg := sandboxCfg(t)

	got, err := createVaultMarkerIfAbsent(cfg, "v_first")
	if err != nil {
		t.Fatal(err)
	}
	if got != "v_first" {
		t.Fatalf("first create: got id %q, want v_first", got)
	}
	if _, err := os.Stat(markerPath(cfg)); err != nil {
		t.Fatalf("marker not written: %v", err)
	}

	// Second create must NOT overwrite — returns the existing id.
	got2, err := createVaultMarkerIfAbsent(cfg, "v_second")
	if err != nil {
		t.Fatal(err)
	}
	if got2 != "v_first" {
		t.Fatalf("second create returned %q, want existing v_first (write-once)", got2)
	}

	m, present, err := readVaultMarker(cfg)
	if err != nil || !present {
		t.Fatalf("read: present=%v err=%v", present, err)
	}
	if m.VaultID != "v_first" || m.Schema != 1 {
		t.Fatalf("marker = %+v", m)
	}

	// B6: the atomic (temp→fsync→rename) write must leave a VALID, complete JSON
	// file on disk — never a torn fragment — and no temp files lying around.
	raw, err := os.ReadFile(markerPath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	var probe vaultMarker
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("marker on disk is not valid JSON: %v\n%s", err, raw)
	}
	if probe.VaultID != "v_first" {
		t.Fatalf("on-disk marker id = %q, want v_first", probe.VaultID)
	}
	entries, err := os.ReadDir(cfg.VaultDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json.tmp") {
			t.Fatalf("a temp marker file was left behind: %s", e.Name())
		}
	}
}

// TestCorruptMarkerFailsLoud covers F2: a corrupt marker must fail loud (it is
// identity-critical), not be silently treated as absent — which would disable
// the rebuild guard exactly when the vault's identity is in doubt.
func TestCorruptMarkerFailsLoud(t *testing.T) {
	cfg := sandboxCfg(t)
	if err := os.WriteFile(markerPath(cfg), []byte("{bad json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := readVaultMarker(cfg)
	if err == nil {
		t.Fatal("readVaultMarker must return an error for a corrupt marker")
	}
	if !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("error should mention 'corrupt'; got: %v", err)
	}
	// A rebuild must surface the corrupt-marker error rather than swallow it.
	if err := writeMemory(cfg, Memory{ID: newID(), Scope: "global", Type: "insight", Title: "x", Source: "manual", CreatedAt: nowRFC3339(), Text: "y"}); err != nil {
		t.Fatal(err)
	}
	if _, rerr := rebuildIndex(context.Background(), cfg); rerr == nil || !strings.Contains(rerr.Error(), "corrupt") {
		t.Fatalf("rebuild should surface the corrupt-marker error; got: %v", rerr)
	}
}

// TestReadIndexVaultIDNoIndex covers F3: reading the index vault id when no index
// exists must return ("", nil), not a hard error. Two flavors: a never-built
// index in an existing data_dir (surfaces as "no such table"), and a data_dir
// that does not exist at all (surfaces as "unable to open database file" — the
// branch the fix adds).
func TestReadIndexVaultIDNoIndex(t *testing.T) {
	cfg := sandboxCfg(t)
	id, err := readIndexVaultID(context.Background(), cfg)
	if err != nil {
		t.Fatalf("readIndexVaultID on a never-built index must not error; got: %v", err)
	}
	if id != "" {
		t.Fatalf("readIndexVaultID = %q, want empty", id)
	}

	// data_dir missing entirely (e.g. wiped) → the open itself fails.
	gone := cfg
	gone.DataDir = filepath.Join(t.TempDir(), "does-not-exist")
	id, err = readIndexVaultID(context.Background(), gone)
	if err != nil {
		t.Fatalf("readIndexVaultID with a missing data_dir must not error; got: %v", err)
	}
	if id != "" {
		t.Fatalf("readIndexVaultID = %q, want empty", id)
	}
}

func TestVaultMarkerAbsent(t *testing.T) {
	cfg := sandboxCfg(t)
	_, present, err := readVaultMarker(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if present {
		t.Fatal("expected no marker in a fresh sandbox")
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
	if err := cmdIndex(context.Background(), []string{"rebuild"}, &out, bytes.NewReader(nil)); !errors.Is(err, errRebuildBlocked) {
		t.Fatalf("want blocked without --force, got %v", err)
	}
	// With --force: succeeds, index empties.
	out.Reset()
	if err := cmdIndex(context.Background(), []string{"rebuild", "--force"}, &out, bytes.NewReader(nil)); err != nil {
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
	if err := cmdInit(context.Background(), nil, &out, bytes.NewReader(nil)); err != nil {
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

func TestAssessRebuild(t *testing.T) {
	cases := []struct {
		name               string
		oldCount, newCount int
		markerID           string
		markerPresent      bool
		indexID            string
		want               rebuildDecision
	}{
		{"first build empty index", 0, 0, "", false, "", decProceed},
		{"first build populating", 0, 5, "", false, "", decProceed},
		{"the incident: populated->empty, no marker", 2823, 0, "", false, "", decBlockEmpty},
		{"populated->empty, marker matches index", 2823, 0, "v_a", true, "v_a", decBlockEmpty},
		{"adopt: legacy vault, no ids anywhere", 2823, 2820, "", false, "", decAdopt},
		{"adopt: marker present, index has no id yet", 10, 10, "v_a", true, "", decAdopt},
		{"block: index knows its id, marker vanished", 10, 10, "", false, "v_a", decBlockIdentity},
		{"block: different vault", 10, 9, "v_b", true, "v_a", decBlockIdentity},
		{"normal rebuild, ids match", 10, 11, "v_a", true, "v_a", decProceed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := assessRebuild(c.oldCount, c.newCount, c.markerID, c.markerPresent, c.indexID)
			if got != c.want {
				t.Fatalf("assessRebuild = %v, want %v", got, c.want)
			}
		})
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
	if err := cmdDelete(context.Background(), []string{"--yes", id}, &out); err != nil {
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
	if _, err := callMCPTool(context.Background(), "delete_memory", map[string]any{"id": id}); err != nil {
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
	if err := cmdDoctor(context.Background(), nil, &txt); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(txt.String(), "BLOCKED") {
		t.Fatalf("doctor text output should mention BLOCKED; got:\n%s", txt.String())
	}

	var js bytes.Buffer
	if err := cmdDoctor(context.Background(), []string{"--json"}, &js); err != nil {
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
	if err := cmdConfig(nil, &out); err != nil {
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

	var out bytes.Buffer
	err := cmdWrite(context.Background(), []string{"--title", "new note", "--text", "do not lose me"}, &out)
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
	if !strings.Contains(out.String(), "warning") || !strings.Contains(out.String(), "index") {
		t.Fatalf("expected a degraded-success warning about the index; got:\n%s", out.String())
	}
}
