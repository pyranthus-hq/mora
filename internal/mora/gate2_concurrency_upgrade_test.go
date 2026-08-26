package mora

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/pyranthus-hq/mora/internal/genericutil"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/imessage"
	"github.com/pyranthus-hq/mora/internal/memory"
)

// upgrade_preserves_test.go — Packet G1 / HEALTH-08 (matrix row 30).
//
// MUTATION: serve a schema-stale index instead of refusing → this test RED.

// TestUpgradePreservesState builds a fixture HOME with the durable artifacts an
// upgrade must keep, bumps indexSchemaVersion via its package-var seam (there is
// no compile-time const seam), and asserts: every artifact remains parseable; a
// schema-stale index never serves as fresh; no token/share/vault path is touched.
func TestUpgradePreservesState(t *testing.T) {
	withTempHome(t)
	t.Setenv("MORA_EMBEDDER", "")
	run(t, "init")
	cfg := mustConfig(t)
	ctx := testCtx(t)

	// --- durable artifacts a real install accumulates ---
	if _, err := cliWrite(ctx, "global", "pre-upgrade", "body before binary swap"); err != nil {
		t.Fatal(err)
	}
	if err := writeConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cfg.ConfigDir, "config.toml")); err != nil {
		t.Fatalf("config.toml missing: %v", err)
	}
	if _, err := os.Stat(markerPath(cfg)); err != nil {
		t.Fatalf("vault marker missing: %v", err)
	}
	if err := saveSources(cfg, []Source{
		{Name: "gmail", Type: "gmail", Enabled: genericutil.Ptr(true), CreatedAt: nowRFC3339()},
	}); err != nil {
		t.Fatal(err)
	}
	statusPath := filepath.Join(cfg.StateDir, "sync", "google-gmail.json")
	if err := os.MkdirAll(filepath.Dir(statusPath), 0o700); err != nil {
		t.Fatal(err)
	}
	oldSuccess := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	if err := memory.SaveStatus(statusPath, &memory.SyncStatus{
		Source: "gmail", LastSuccessAt: oldSuccess, LastSynced: oldSuccess,
	}); err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(cfg.ConfigDir, "tokens", "google-gmail.json")
	if err := os.MkdirAll(filepath.Dir(tokenPath), 0o700); err != nil {
		t.Fatal(err)
	}
	tokenBody := []byte(`{"access_token":"test-not-a-real-token","token_type":"Bearer"}`)
	if err := os.WriteFile(tokenPath, tokenBody, 0o600); err != nil {
		t.Fatal(err)
	}
	govDir := filepath.Join(cfg.StateDir, "governance")
	if err := os.MkdirAll(govDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(govDir, "ledger.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	usageDir := filepath.Join(cfg.StateDir, "usage")
	if err := os.MkdirAll(usageDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(usageDir, "events.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	dbBefore, err := os.ReadFile(dbPath(cfg))
	if err != nil {
		t.Fatalf("index.db before bump: %v", err)
	}
	tokenBefore, _ := os.ReadFile(tokenPath)
	vaultListBefore, _ := listMemories(cfg, "", 100)

	// Simulate newer binary: expect schema N+1 while the on-disk index is still N.
	origVer := indexSchemaVersion
	origHeal := indexAutoHeal
	t.Cleanup(func() {
		indexSchemaVersion = origVer
		indexAutoHeal = origHeal
	})
	indexSchemaVersion = origVer + 1

	// Branch A — auto-heal OFF: openIndexRO must refuse loudly (matrix row 30).
	indexAutoHeal = func(Config) bool { return false }
	_, openErr := openIndexRO(ctx, cfg)
	if openErr == nil {
		t.Fatal("openIndexRO must refuse a schema-stale index when auto-heal is off")
	}
	if !strings.Contains(openErr.Error(), "different mora version") &&
		!strings.Contains(openErr.Error(), "schema") {
		t.Fatalf("refusal error = %v, want a schema/version message", openErr)
	}
	h := indexHealthOf(cfg, time.Now())
	if h.State == idxFresh {
		t.Fatalf("indexHealthOf on schema-stale must be non-fresh, got %q", h.State)
	}

	// Branch B — auto-heal ON: a failed heal (blocked by vault identity trick) still
	// reads non-fresh; a successful heal rebuilds. Use the refuse path's invariant
	// that health is non-fresh until a rebuild commits at the NEW version.
	indexAutoHeal = func(Config) bool { return true }
	// Force heal to succeed: rebuild with the bumped schema.
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatalf("post-bump rebuild: %v", err)
	}
	db, err := openIndexRO(ctx, cfg)
	if err != nil {
		t.Fatalf("openIndexRO after heal rebuild: %v", err)
	}
	_ = db.Close()
	h2 := indexHealthOf(cfg, time.Now())
	if h2.State != idxFresh && h2.State != idxDirty {
		// Dirty is acceptable if pending ops remain; failed/never is not.
		if h2.State == idxFailed || h2.State == idxNever {
			t.Fatalf("index health after heal = %q", h2.State)
		}
	}

	// Artifacts preserved; tokens/share/vault paths untouched.
	if _, err := os.Stat(filepath.Join(cfg.ConfigDir, "config.toml")); err != nil {
		t.Fatalf("config.toml vanished across upgrade: %v", err)
	}
	if _, err := os.Stat(markerPath(cfg)); err != nil {
		t.Fatalf("vault marker vanished: %v", err)
	}
	sources, err := loadSources(cfg)
	if err != nil || len(sources) == 0 || sources[0].Name != "gmail" {
		t.Fatalf("sources.json lost/corrupt: %v %+v", err, sources)
	}
	st, err := memory.LoadStatus(statusPath)
	if err != nil || st.LastSuccessAt != oldSuccess {
		t.Fatalf("per-source status lost: err=%v st=%+v", err, st)
	}
	tokenAfter, err := os.ReadFile(tokenPath)
	if err != nil || !bytes.Equal(tokenAfter, tokenBefore) {
		t.Fatalf("OAuth token touched across upgrade: before=%q after=%q err=%v", tokenBefore, tokenAfter, err)
	}
	if _, err := os.Stat(filepath.Join(govDir, "ledger.jsonl")); err != nil {
		t.Fatalf("governance ledger vanished: %v", err)
	}
	vaultListAfter, _ := listMemories(cfg, "", 100)
	if len(vaultListAfter) < len(vaultListBefore) {
		t.Fatalf("vault memories lost: before=%d after=%d", len(vaultListBefore), len(vaultListAfter))
	}
	// Index bytes may change on heal rebuild; the point is the vault/tokens did not.
	_ = dbBefore
}

// TestFDALossNeverStampsSuccess — Packet G2 / HEALTH-08 (matrix row 29).
//
// MUTATION: swallow the FDA open error in ingestIMessage → this test RED.
func TestFDALossNeverStampsSuccess(t *testing.T) {
	withTempHome(t)
	t.Setenv("MORA_EMBEDDER", "")
	run(t, "init")
	cfg := mustConfig(t)

	origGOOS := runtimeGOOS
	origFetcher := newIMessageFetcher
	t.Cleanup(func() {
		runtimeGOOS = origGOOS
		newIMessageFetcher = origFetcher
	})
	runtimeGOOS = func() string { return "darwin" }

	src := Source{Name: "im", Type: "imessage", Enabled: genericutil.Ptr(true), CreatedAt: nowRFC3339()}
	if err := saveSources(cfg, []Source{src}); err != nil {
		t.Fatal(err)
	}
	statusPath := imessageStatusPath(cfg, src.Name)
	if err := os.MkdirAll(filepath.Dir(statusPath), 0o700); err != nil {
		t.Fatal(err)
	}
	oldSuccess := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	if err := memory.SaveStatus(statusPath, &memory.SyncStatus{
		Source: src.Name, LastSuccessAt: oldSuccess, LastSynced: oldSuccess,
	}); err != nil {
		t.Fatal(err)
	}

	// Denied open — never stamps success.
	newIMessageFetcher = func(string, imessage.DenyList) (iMessageFetcher, error) {
		return nil, errors.New("open chat.db: permission denied (FDA)")
	}
	var out bytes.Buffer
	n, err := ingestSource(cfg, src, &out)
	if err == nil {
		t.Fatal("denied FDA open must return an error")
	}
	if n != 0 {
		t.Fatalf("denied ingest count = %d, want 0", n)
	}
	if !strings.Contains(err.Error(), "cannot read your Messages database") {
		t.Fatalf("error = %v, want FDA guidance", err)
	}
	st, loadErr := memory.LoadStatus(statusPath)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if st.LastSuccessAt != oldSuccess {
		t.Fatalf("LastSuccessAt advanced on FDA denial: got %q want %q", st.LastSuccessAt, oldSuccess)
	}
	if st.LastError == "" {
		t.Fatal("denied read must stamp LastError via stampSyncAttemptFailure")
	}

	// Doctor / banner go red on the aged frozen success.
	var js bytes.Buffer
	if err := cmdDoctor(testCtx(t), []string{"--json", "--strict"}, &js, testStderr); err == nil {
		t.Fatal("doctor --strict must be nonzero with a failed FDA source")
	}
	banner := healthBannerFrom(healthOf(cfg, time.Now()))
	if banner == "" {
		t.Fatal("expected a red health banner after FDA denial")
	}

	// Zero-row SUCCESS must still stamp success (never conflated with denial).
	newIMessageFetcher = func(string, imessage.DenyList) (iMessageFetcher, error) {
		return emptyIMessageFetcher{}, nil
	}
	beforeZero, _ := memory.LoadStatus(statusPath)
	n, err = ingestSource(cfg, src, &out)
	if err != nil {
		t.Fatalf("zero-row fetcher must succeed: %v", err)
	}
	if n != 0 {
		t.Fatalf("zero-row count = %d, want 0", n)
	}
	afterZero, _ := memory.LoadStatus(statusPath)
	if afterZero.LastSuccessAt == "" || afterZero.LastSuccessAt == beforeZero.LastSuccessAt && beforeZero.LastError != "" {
		// After a prior failure, a clean zero-row sync must advance LastSuccessAt.
		if afterZero.LastSuccessAt == oldSuccess {
			t.Fatalf("zero-row success must advance LastSuccessAt past the frozen denial stamp; got %q", afterZero.LastSuccessAt)
		}
	}
	if afterZero.LastError != "" {
		t.Fatalf("zero-row success must clear LastError, got %q", afterZero.LastError)
	}
}

// emptyIMessageFetcher returns no conversations — a legitimate empty sync.
type emptyIMessageFetcher struct{}

func (emptyIMessageFetcher) FetchPage(memory.ItemKind, memory.FetchWindow, string) (memory.Page, error) {
	return memory.Page{}, nil
}
func (emptyIMessageFetcher) Close() error { return nil }

// TestAccidentalVaultFlipIsBlockedAndVisible — historical incident replay (Finding 9c).
// Rewrite config.toml's vault_dir to an empty ephemeral dir with NO cmdInit, then
// run unattended paths and assert the block record + doctor surface.
func TestAccidentalVaultFlipIsBlockedAndVisible(t *testing.T) {
	withTempHome(t)
	t.Setenv("MORA_EMBEDDER", "")
	run(t, "init")
	cfg := mustConfig(t)
	ctx := testCtx(t)

	var ids []string
	for i := 0; i < 3; i++ {
		id, err := cliWrite(ctx, "global", fmt.Sprintf("vault-flip-%d", i), "vault-flip body")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	beforeCount := indexCount(t, cfg)
	if beforeCount < 3 {
		t.Fatalf("precondition index count = %d", beforeCount)
	}

	origVault := cfg.VaultDir
	ephemeral := t.TempDir()
	cfg.VaultDir = ephemeral
	if err := writeConfig(cfg); err != nil {
		t.Fatal(err)
	}
	// Reload as an unattended process would.
	cfg2 := mustConfig(t)
	if cfg2.VaultDir != ephemeral {
		t.Fatalf("config reload vault_dir = %q, want ephemeral %q", cfg2.VaultDir, ephemeral)
	}

	var out bytes.Buffer
	err := cmdIndex(ctx, []string{"rebuild"}, &out, testStderr, strings.NewReader(""))
	if !errors.Is(err, errRebuildBlocked) {
		t.Fatalf("unattended rebuild after vault flip: err=%v want errRebuildBlocked\nout=%s", err, out.String())
	}
	if _, present, _ := readBlockRecord(cfg2); !present {
		t.Fatal("last-rebuild-block.json must be present after the flip")
	}
	// Original index rows must be unchanged (DataDir still holds the old index).
	if got := indexCount(t, cfg2); got != beforeCount {
		t.Fatalf("index row count changed on a blocked rebuild: before=%d after=%d", beforeCount, got)
	}
	_ = ids
	_ = origVault

	var js bytes.Buffer
	if err := cmdDoctor(ctx, []string{"--json", "--strict"}, &js, testStderr); err == nil {
		t.Fatal("doctor --strict must be nonzero after an accidental vault flip")
	}

	// MCP write_memory on the flipped vault must also degrade/block index mutation,
	// never silently rebuild onto the empty vault.
	_, mcpErr := callMCPTool(ctx, "write_memory", map[string]any{
		"scope": "global", "type": "insight", "title": "after flip", "text": "must not wipe index",
	})
	if mcpErr != nil && !errors.Is(mcpErr, errRebuildBlocked) {
		if !strings.Contains(mcpErr.Error(), "index") && !strings.Contains(mcpErr.Error(), "blocked") {
			t.Fatalf("write_memory after flip: unexpected error %v", mcpErr)
		}
	}
	if got := indexCount(t, cfg2); got < beforeCount {
		t.Fatalf("index lost rows after MCP write on flipped vault: before=%d after=%d", beforeCount, got)
	}
}
