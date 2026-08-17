package mora

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/pyranthus-hq/mora/internal/memory"
)

func TestSyncRequiresExplicitKnownSource(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	for _, args := range [][]string{{"sync"}, {"sync", "bogus"}} {
		out, err := runErr(t, args...)
		if err == nil {
			t.Fatalf("Run(%v) succeeded; want an explicit-source error", args)
		}
		if strings.Contains(out, "Google sign-in") || strings.Contains(out, "sync incomplete") {
			t.Fatalf("Run(%v) entered a connector backfill:\n%s", args, out)
		}
	}

	help := run(t, "sync", "--help")
	if !strings.Contains(help, "sync <status|google|github|filesystem|imessage|applecalendar|git>") {
		t.Fatalf("sync help does not advertise every supported route:\n%s", help)
	}
}

func TestSyncFilesystemReindexesOnlyEnabledFilesystemSources(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	t.Setenv("MORA_GOOGLE_CREDENTIALS", "")

	enabledDir := t.TempDir()
	disabledDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(enabledDir, "enabled.md"), []byte("# Enabled\ncedarenabledmarker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(disabledDir, "disabled.md"), []byte("# Disabled\ncedardisabledmarker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := saveSources(cfg, []Source{
		// An enabled network source makes the regression observable: the old
		// fallthrough attempted Google instead of walking the filesystem source.
		{Name: "gmail", Type: "gmail", Scope: "personal", Enabled: ptr(true), CreatedAt: nowRFC3339()},
		{Name: "docs", Type: "filesystem", Path: enabledDir, Scope: "personal", Enabled: ptr(true), CreatedAt: nowRFC3339()},
		{Name: "archive", Type: "filesystem", Path: disabledDir, Scope: "personal", Enabled: ptr(false), CreatedAt: nowRFC3339()},
	}); err != nil {
		t.Fatal(err)
	}

	out := run(t, "sync", "filesystem")
	if !strings.Contains(out, "synced 1 item(s)") {
		t.Fatalf("sync filesystem did not report the enabled file:\n%s", out)
	}
	if strings.Contains(out, "Google sign-in") || strings.Contains(out, "sync incomplete") {
		t.Fatalf("sync filesystem entered the Google route:\n%s", out)
	}

	assertSearchCount := func(query string, want int) {
		t.Helper()
		raw := run(t, "search", query, "--json")
		// Plan 01-07: `search --json` carries its array under `memories`.
		got, err := decodeMemoriesJSON(t, raw)
		if err != nil {
			t.Fatalf("decode search %q: %v\n%s", query, err, raw)
		}
		if len(got) != want {
			t.Fatalf("search %q returned %d memories, want %d: %+v", query, len(got), want, got)
		}
	}
	assertSearchCount("cedarenabledmarker", 1)
	assertSearchCount("cedardisabledmarker", 0)
}

func TestSyncFilesystemSurfacesCorruptSourceRegistry(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	if err := os.WriteFile(filepath.Join(cfg.ConfigDir, "sources.json"), []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := runErr(t, "sync", "filesystem")
	if err == nil || !strings.Contains(err.Error(), "load sources") {
		t.Fatalf("sync filesystem must surface a corrupt source registry, got %v", err)
	}
}

func TestSyncFilesystemContinuesAfterSourceWalkError(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	healthyDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(healthyDir, "healthy.md"), []byte("healthyaftermissingmarker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := Source{
		Name: "gone", Type: "filesystem", Path: filepath.Join(t.TempDir(), "does-not-exist"),
		Scope: "personal", Enabled: ptr(true), CreatedAt: nowRFC3339(),
	}
	healthy := Source{
		Name: "healthy", Type: "filesystem", Path: healthyDir,
		Scope: "personal", Enabled: ptr(true), CreatedAt: nowRFC3339(),
	}
	if err := saveSources(cfg, []Source{missing, healthy}); err != nil {
		t.Fatal(err)
	}

	out, err := runErr(t, "sync", "filesystem")
	if err == nil || !strings.Contains(err.Error(), "1 source(s) failed to sync") {
		t.Fatalf("mixed filesystem sync must return the aggregate failure, got %v\n%s", err, out)
	}
	if !strings.Contains(out, "gone sync incomplete") || !strings.Contains(out, "synced 1 item(s)") {
		t.Fatalf("mixed filesystem sync did not warn and continue:\n%s", out)
	}

	raw := run(t, "search", "healthyaftermissingmarker", "--json")
	got, err := decodeMemoriesJSON(t, raw) // Plan 01-07: array under `memories`
	if err != nil || len(got) != 1 {
		t.Fatalf("healthy source was not indexed after the earlier walk error: got=%+v err=%v\n%s", got, err, raw)
	}
	failedStatus, err := memory.LoadStatus(syncStatusPathFor(cfg, missing))
	if err != nil {
		t.Fatal(err)
	}
	if failedStatus.LastError == "" || failedStatus.LastAttemptAt == "" || failedStatus.LastSuccessAt != "" || failedStatus.LastSynced != "" {
		t.Fatalf("failed source status is dishonest: %+v", failedStatus)
	}
	healthyStatus, err := memory.LoadStatus(syncStatusPathFor(cfg, healthy))
	if err != nil {
		t.Fatal(err)
	}
	if healthyStatus.LastSuccessAt == "" || healthyStatus.LastError != "" {
		t.Fatalf("healthy source did not complete after prior failure: %+v", healthyStatus)
	}
}

func TestSyncFilesystemUnreadableSelectedFilePreservesLastSuccess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("broken symlink fixture requires POSIX symlink semantics")
	}
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	root := t.TempDir()
	path := filepath.Join(root, "selected.md")
	if err := os.WriteFile(path, []byte("priorreadablemarker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := Source{Name: "docs", Type: "filesystem", Path: root, Scope: "personal", Enabled: ptr(true), CreatedAt: nowRFC3339()}
	if err := saveSources(cfg, []Source{source}); err != nil {
		t.Fatal(err)
	}
	run(t, "sync", "filesystem")
	statusPath := syncStatusPathFor(cfg, source)
	before, err := memory.LoadStatus(statusPath)
	if err != nil || before.LastSuccessAt == "" {
		t.Fatalf("initial successful status: %+v err=%v", before, err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "missing-target"), path); err != nil {
		t.Fatal(err)
	}
	out, err := runErr(t, "sync", "filesystem")
	if err == nil || !strings.Contains(err.Error(), "failed to sync") || !strings.Contains(out, "reading filesystem source") {
		t.Fatalf("unreadable selected file was reported fresh: err=%v\n%s", err, out)
	}
	after, err := memory.LoadStatus(statusPath)
	if err != nil {
		t.Fatal(err)
	}
	if after.LastSuccessAt != before.LastSuccessAt || after.LastSynced != before.LastSynced {
		t.Fatalf("failed file read advanced success: before=%+v after=%+v", before, after)
	}
	if after.LastError == "" || after.ErrorCount == 0 || after.LastAttemptAt == "" {
		t.Fatalf("failed file read was not surfaced in status: %+v", after)
	}

	got, err := decodeMemoriesJSON(t, run(t, "search", "priorreadablemarker", "--json")) // Plan 01-07
	if err != nil || len(got) != 1 {
		t.Fatalf("last-good memory should remain searchable under failed health: got=%+v err=%v", got, err)
	}
}
