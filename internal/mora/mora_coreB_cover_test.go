package mora

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// mora_coreB_cover_test.go — coordinator gap-filler for the coreB area
// (internal/mora/mora.go funcs at line >= 3025). It closes the REAL, reachable
// logic branches the six per-group workers left uncovered (as opposed to the
// unforceable I/O/driver/live-network/non-darwin error guards they documented).
// Namespace: every test is TestCoreB_Gap*, every helper is coreBGap*.

// coreBGapInitCfg gives a fully-scaffolded vault + config rooted at a temp HOME.
func coreBGapInitCfg(t *testing.T) Config {
	t.Helper()
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig after init: %v", err)
	}
	return cfg
}

// TestCoreB_GapAddSourceKeepsOtherNames covers addSource's registry-merge loop
// (the `existing.Name != s.Name` retain branch): a second add must PRESERVE the
// first, differently-named source rather than replacing the whole registry.
func TestCoreB_GapAddSourceKeepsOtherNames(t *testing.T) {
	cfg := coreBGapInitCfg(t)
	if err := addSource(cfg, []string{"filesystem", "--name", "aaa", "--path", "/tmp/coreb-aaa"}, io.Discard); err != nil {
		t.Fatalf("addSource aaa: %v", err)
	}
	if err := addSource(cfg, []string{"filesystem", "--name", "bbb", "--path", "/tmp/coreb-bbb"}, io.Discard); err != nil {
		t.Fatalf("addSource bbb: %v", err)
	}
	sources, err := loadSources(cfg)
	if err != nil {
		t.Fatalf("loadSources: %v", err)
	}
	names := map[string]bool{}
	for _, s := range sources {
		names[s.Name] = true
	}
	if !names["aaa"] || !names["bbb"] {
		t.Fatalf("expected both aaa and bbb to survive the second add, got %v", names)
	}
	// A freshly added source is consent-gated (Enabled explicitly false).
	for _, s := range sources {
		if s.Name == "aaa" || s.Name == "bbb" {
			if s.IsEnabled() {
				t.Fatalf("source %q should be added disabled (consent-gated), got enabled", s.Name)
			}
		}
	}
}

// TestCoreB_GapAppleCalDBPathPrefersModern covers appleCalDBPath's
// modern-store-exists branch: once the modern group-container DB is present it is
// returned directly, without probing the legacy location.
func TestCoreB_GapAppleCalDBPathPrefersModern(t *testing.T) {
	withTempHome(t)
	// With neither store present, the function returns the modern default path.
	modern := appleCalDBPath()
	if modern == "" {
		t.Fatal("appleCalDBPath returned empty path")
	}
	// Materialize the modern store, then re-probe: the modern-exists branch fires.
	if err := os.MkdirAll(filepath.Dir(modern), 0o755); err != nil {
		t.Fatalf("mkdir modern parent: %v", err)
	}
	if err := os.WriteFile(modern, []byte("sqlite"), 0o644); err != nil {
		t.Fatalf("write modern db: %v", err)
	}
	if got := appleCalDBPath(); got != modern {
		t.Fatalf("appleCalDBPath should return the existing modern store %q, got %q", modern, got)
	}
}

// TestCoreB_GapConnectIMessagePersistsSinceDays covers connectIMessage's
// `--since-days` override branch: a non-zero window is persisted on the imessage
// source so future syncs reuse it (mirrors `connect google --since-days`).
func TestCoreB_GapConnectIMessagePersistsSinceDays(t *testing.T) {
	asDarwinOnWindows(t)
	cfg := coreBGapInitCfg(t)
	var out bytes.Buffer
	// On this darwin host the local Messages DB is absent under the temp HOME, so
	// printIMessageReadiness stops before any backfill and connect returns nil; the
	// since-days write happens first regardless. We assert the persisted side effect
	// (and tolerate a readiness-dependent error).
	_ = connectIMessage(context.Background(), []string{"--since-days", "-5"}, &out)
	if !strings.Contains(out.String(), "enabled imessage") {
		t.Fatalf("expected connect output to confirm imessage enabled, got: %s", out.String())
	}
	sources, err := loadSources(cfg)
	if err != nil {
		t.Fatalf("loadSources: %v", err)
	}
	found := false
	for _, s := range sources {
		if s.Type == "imessage" {
			found = true
			if s.SinceDays != -5 {
				t.Fatalf("imessage source SinceDays=%d, want -5 (all-time override persisted)", s.SinceDays)
			}
			if !s.IsEnabled() {
				t.Fatalf("imessage source should be enabled after connect")
			}
		}
	}
	if !found {
		t.Fatal("no imessage source was registered by connectIMessage")
	}
}

// TestCoreB_GapConnectFilesystemRejectsBadFlag covers connectFilesystem's
// flag-parse error branch: an unknown flag after the positional path aborts with
// an error rather than registering a half-parsed source.
func TestCoreB_GapConnectFilesystemRejectsBadFlag(t *testing.T) {
	coreBGapInitCfg(t)
	dir := t.TempDir()
	err := connectFilesystem(context.Background(), []string{dir, "--this-flag-does-not-exist"}, io.Discard)
	if err == nil {
		t.Fatal("expected connectFilesystem to reject an unknown flag, got nil error")
	}
	if !strings.Contains(err.Error(), "not defined") && !strings.Contains(err.Error(), "flag") {
		t.Fatalf("expected a flag-parse error, got: %v", err)
	}
}

// TestCoreB_GapBackfillEnabledIMessageProcessesEnabledSource covers
// backfillEnabledIMessage's non-skip loop body (an enabled imessage source is
// actually ingested) and, on darwin, the failures>0 aggregation branch (the local
// Messages DB is unreadable under the temp HOME, so the sync is surfaced as failed
// rather than swallowed). On non-darwin the connector no-ops to (0,nil).
func TestCoreB_GapBackfillEnabledIMessageProcessesEnabledSource(t *testing.T) {
	cfg := coreBGapInitCfg(t)
	if err := setSourceEnabled(cfg, "imessage", true); err != nil {
		t.Fatalf("setSourceEnabled imessage: %v", err)
	}
	var out bytes.Buffer
	total, err := backfillEnabledIMessage(context.Background(), cfg, &out)
	if total != 0 {
		t.Fatalf("expected 0 items backfilled from an unreadable/absent Messages DB, got %d", total)
	}
	if runtime.GOOS == "darwin" {
		if err == nil {
			t.Fatal("on darwin an unreadable Messages DB must surface as a sync failure, got nil error")
		}
		if !strings.Contains(err.Error(), "failed to sync") {
			t.Fatalf("expected an aggregated sync-failure error, got: %v", err)
		}
	} else if err != nil {
		t.Fatalf("on %s the imessage connector should no-op cleanly, got: %v", runtime.GOOS, err)
	}
}
