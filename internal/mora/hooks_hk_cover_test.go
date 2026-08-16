package mora

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hkSetExecutable swaps the hookExecutable seam for the duration of a test and
// restores it afterwards, so install/uninstall exercise a controlled exe path.
func hkSetExecutable(t *testing.T, fn func() (string, error)) {
	t.Helper()
	prev := hookExecutable
	hookExecutable = fn
	t.Cleanup(func() { hookExecutable = prev })
}

// hkBreakLoadConfig points MORA_CONFIG_DIR at a dir whose config.toml is itself
// a directory, so loadConfig()'s os.ReadFile returns a non-NotExist error — the
// fail-soft branch every hook/CLI command takes when config is unreadable.
func hkBreakLoadConfig(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("MORA_CONFIG_DIR", dir)
	if err := os.Mkdir(filepath.Join(dir, "config.toml"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// hkSetSearch swaps the hookSearchMemories seam and restores it after the test.
// The trailing ...searchFilters mirrors searchMemories' #241 optional filter
// param (hookSearchMemories is var-inferred from searchMemories' own type).
func hkSetSearch(t *testing.T, fn func(context.Context, Config, string, string, int, ...searchFilters) ([]Memory, error)) {
	t.Helper()
	prev := hookSearchMemories
	hookSearchMemories = fn
	t.Cleanup(func() { hookSearchMemories = prev })
}

// ---------------------------------------------------------------------------
// JSON (un)marshal seams
// ---------------------------------------------------------------------------

// TestHk_ClaudeCommandHookUnmarshalError asserts the custom UnmarshalJSON
// surfaces a decode error when a typed field carries the wrong JSON type.

// TestHk_ClaudeHookGroupUnmarshalError asserts the group decoder rejects a
// wrong-typed matcher and preserves unknown group-level fields.

// ---------------------------------------------------------------------------
// cmdHook dispatch
// ---------------------------------------------------------------------------

// TestHk_CmdHookUsageErrors covers the no-subcommand and unknown-subcommand
// usage errors.
func TestHk_CmdHookUsageErrors(t *testing.T) {
	var out bytes.Buffer
	if err := cmdHook(context.Background(), nil, &out, strings.NewReader("")); err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Fatalf("empty args must return a usage error, got: %v", err)
	}
	out.Reset()
	if err := cmdHook(context.Background(), []string{"frobnicate"}, &out, strings.NewReader("")); err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Fatalf("unknown subcommand must return a usage error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// hookSessionStart fail-soft paths
// ---------------------------------------------------------------------------

// TestHk_HookSessionStartInvalidJSON asserts a malformed stdin is swallowed
// (fail-open: a hook must never break the session) with no output.
func TestHk_HookSessionStartInvalidJSON(t *testing.T) {
	withTempHome(t)
	var out bytes.Buffer
	if err := cmdHook(context.Background(), []string{"session-start"}, &out, strings.NewReader("this is not json")); err != nil {
		t.Fatalf("invalid stdin must fail open, got: %v", err)
	}
	if out.String() != "" {
		t.Fatalf("invalid stdin must emit nothing, got %q", out.String())
	}
}

// TestHk_HookSessionStartLoadConfigError asserts an unreadable config fails open.
func TestHk_HookSessionStartLoadConfigError(t *testing.T) {
	hkBreakLoadConfig(t)
	var out bytes.Buffer
	if err := cmdHook(context.Background(), []string{"session-start"}, &out, strings.NewReader(`{"source":"startup"}`)); err != nil {
		t.Fatalf("unreadable config must fail open, got: %v", err)
	}
	if out.String() != "" {
		t.Fatalf("config error must emit nothing, got %q", out.String())
	}
}

// ---------------------------------------------------------------------------
// hookRecall fail-soft paths
// ---------------------------------------------------------------------------

// TestHk_HookRecallBadFlag asserts an unparseable --threshold makes recall a
// silent no-op (parseHookRecallArgs returns ok=false).
func TestHk_HookRecallBadFlag(t *testing.T) {
	withTempHome(t)
	var out bytes.Buffer
	err := cmdHook(context.Background(), []string{"recall", "--threshold", "not-a-float"}, &out,
		strings.NewReader(`{"prompt":"a sufficiently long prompt here"}`))
	if err != nil {
		t.Fatalf("bad flag must be swallowed, got: %v", err)
	}
	if out.String() != "" {
		t.Fatalf("bad flag must emit nothing, got %q", out.String())
	}
}

// TestHk_HookRecallInvalidJSON asserts malformed stdin (with valid args) is a
// silent no-op.
func TestHk_HookRecallInvalidJSON(t *testing.T) {
	withTempHome(t)
	var out bytes.Buffer
	if err := cmdHook(context.Background(), []string{"recall"}, &out, strings.NewReader("{not json")); err != nil {
		t.Fatalf("invalid stdin must fail open, got: %v", err)
	}
	if out.String() != "" {
		t.Fatalf("invalid stdin must emit nothing, got %q", out.String())
	}
}

// TestHk_HookRecallLoadConfigError asserts an unreadable config fails open even
// with a valid, recall-worthy prompt.
func TestHk_HookRecallLoadConfigError(t *testing.T) {
	hkBreakLoadConfig(t)
	var out bytes.Buffer
	if err := cmdHook(context.Background(), []string{"recall"}, &out, strings.NewReader(`{"prompt":"remember the eelpout decision please"}`)); err != nil {
		t.Fatalf("unreadable config must fail open, got: %v", err)
	}
	if out.String() != "" {
		t.Fatalf("config error must emit nothing, got %q", out.String())
	}
}

// TestHk_HookRecallSearchError asserts a search failure is swallowed (no context
// is better than a broken prompt).
func TestHk_HookRecallSearchError(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	hkSetSearch(t, func(context.Context, Config, string, string, int, ...searchFilters) ([]Memory, error) {
		return nil, errors.New("index unavailable")
	})
	var out bytes.Buffer
	if err := cmdHook(context.Background(), []string{"recall"}, &out, strings.NewReader(`{"prompt":"what did we decide about the launch"}`)); err != nil {
		t.Fatalf("search error must fail open, got: %v", err)
	}
	if out.String() != "" {
		t.Fatalf("search error must emit nothing, got %q", out.String())
	}
}

// ---------------------------------------------------------------------------
// skipRecallPrompt
// ---------------------------------------------------------------------------

// TestHk_SkipRecallPromptStopwordAfterLength asserts a stopword padded past the
// 12-rune floor still short-circuits via the lowercase/trim switch (the branch
// the cheap-prompt test cannot reach because its stopwords are all < 12 runes).

// ---------------------------------------------------------------------------
// formatRecallContext / recallLine / clipRunes / memoryAge
// ---------------------------------------------------------------------------

// TestHk_FormatRecallContextSkipsBlankLine asserts a memory that renders to an
// empty line (no text and no title) is skipped while a real one is kept.

// TestHk_FormatRecallContextByteLimit asserts the running byte cap stops
// appending once the next line would exceed hookRecallByteLimit, while keeping
// at least the first line.

// TestHk_RecallLineFallbacks exercises every fallback branch of recallLine.

// TestHk_ClipRunes covers the truncation loop and trailing-space trim.

// TestHk_MemoryAge covers every arm of the age formatter.

// ---------------------------------------------------------------------------
// hookInstall / hookUninstall / hookStatus error paths
// ---------------------------------------------------------------------------

// TestHk_HookInstallBadFlag asserts install surfaces a flag-parse error.
func TestHk_HookInstallBadFlag(t *testing.T) {
	if _, err := runErr(t, "hook", "install", "--threshold", "abc"); err == nil {
		t.Fatal("install with an unparseable --threshold must error")
	}
}

// TestHk_HookInstallExecutableError asserts install surfaces a failure to
// resolve the running binary.
func TestHk_HookInstallExecutableError(t *testing.T) {
	withTempHome(t)
	hkSetExecutable(t, func() (string, error) { return "", errors.New("no exe") })
	if err := hookInstall(nil, io.Discard); err == nil || !strings.Contains(err.Error(), "no exe") {
		t.Fatalf("install must surface an executable-resolution error, got: %v", err)
	}
}

// TestHk_HookInstallSettingsPathError asserts install surfaces a home-dir
// resolution failure (claudeSettingsPath -> os.UserHomeDir).
func TestHk_HookInstallSettingsPathError(t *testing.T) {
	hkSetExecutable(t, func() (string, error) { return "/opt/mora/mora", nil })
	setTestHome(t, "")
	if err := hookInstall(nil, io.Discard); err == nil {
		t.Fatal("install with no HOME must surface a settings-path error")
	}
}

// TestHk_HookInstallWriteError asserts install surfaces a settings-write failure
// (the .claude parent is a regular file, so the atomic write cannot stage).
func TestHk_HookInstallWriteError(t *testing.T) {
	tmp := t.TempDir()
	setTestHome(t, tmp)
	t.Setenv("MORA_CONFIG_DIR", "")
	hkSetExecutable(t, func() (string, error) { return filepath.Join(tmp, "bin", "mora"), nil })
	if err := os.WriteFile(filepath.Join(tmp, ".claude"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := hookInstall(nil, io.Discard); err == nil {
		t.Fatal("install must fail when the settings file cannot be written")
	}
}

// TestHk_HookUninstallSettingsPathError asserts uninstall surfaces a home-dir
// resolution failure.
func TestHk_HookUninstallSettingsPathError(t *testing.T) {
	setTestHome(t, "")
	if err := hookUninstall(io.Discard); err == nil {
		t.Fatal("uninstall with no HOME must surface a settings-path error")
	}
}

// TestHk_HookUninstallMalformedHooks asserts uninstall refuses to proceed on a
// malformed hooks block rather than clobbering it.
func TestHk_HookUninstallMalformedHooks(t *testing.T) {
	tmp := withTempHookHome(t)
	writeClaudeSettingsFixture(t, tmp, `{"hooks":"not an object","theme":"dark"}`+"\n")
	if err := hookUninstall(io.Discard); err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("uninstall must surface malformed hooks, got: %v", err)
	}
}

// TestHk_HookUninstallWriteError asserts uninstall surfaces a settings-write
// failure (the .claude parent is a regular file).
func TestHk_HookUninstallWriteError(t *testing.T) {
	tmp := t.TempDir()
	setTestHome(t, tmp)
	t.Setenv("MORA_CONFIG_DIR", "")
	if err := os.WriteFile(filepath.Join(tmp, ".claude"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := hookUninstall(io.Discard); err == nil {
		t.Fatal("uninstall must fail when the settings file cannot be written")
	}
}

// TestHk_HookStatusSettingsPathError asserts status surfaces a home-dir failure.
func TestHk_HookStatusSettingsPathError(t *testing.T) {
	setTestHome(t, "")
	if err := hookStatus(io.Discard); err == nil {
		t.Fatal("status with no HOME must surface a settings-path error")
	}
}

// TestHk_HookStatusMalformedHooks asserts status surfaces a malformed hooks
// block rather than silently reporting "not installed".
func TestHk_HookStatusMalformedHooks(t *testing.T) {
	tmp := withTempHookHome(t)
	writeClaudeSettingsFixture(t, tmp, `{"hooks":42}`+"\n")
	if err := hookStatus(io.Discard); err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("status must surface malformed hooks, got: %v", err)
	}
}

// TestHk_LoadClaudeSettingsMalformedTopLevel asserts a settings.json that is
// not valid JSON at all surfaces a parse error instead of being silently
// treated as empty settings. (Treating it as empty is exactly the wipe hazard:
// install would then rewrite the file with only mora hooks.)
