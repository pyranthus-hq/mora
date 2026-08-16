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
	"time"
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
func TestHk_SkipRecallPromptStopwordAfterLength(t *testing.T) {
	if !skipRecallPrompt("      continue      ") {
		t.Fatal("a padded stopword must still be skipped via the trim/lowercase switch")
	}
	// A genuine question of the same length must NOT be skipped (the default arm).
	if skipRecallPrompt("what should we do about the migration") {
		t.Fatal("a real prompt must not be skipped")
	}
	// Long slash-command still skipped by the leading-slash guard.
	if !skipRecallPrompt("/compact the whole conversation now") {
		t.Fatal("a slash command must be skipped")
	}
}

// ---------------------------------------------------------------------------
// formatRecallContext / recallLine / clipRunes / memoryAge
// ---------------------------------------------------------------------------

// TestHk_FormatRecallContextSkipsBlankLine asserts a memory that renders to an
// empty line (no text and no title) is skipped while a real one is kept.
func TestHk_FormatRecallContextSkipsBlankLine(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	mems := []Memory{
		{ID: "blank", Source: "test", CreatedAt: now.Format(time.RFC3339), Score: -1}, // no text/title -> blank line
		{ID: "real", Title: "Real one", Source: "test", CreatedAt: now.Format(time.RFC3339), Text: "keep me", Score: -1},
	}
	got := formatRecallContext(mems, 0, now)
	if strings.Contains(got, "id: blank") {
		t.Fatalf("blank-line memory must be skipped, got:\n%s", got)
	}
	if !strings.Contains(got, "id: real") {
		t.Fatalf("real memory must be included, got:\n%s", got)
	}
}

// TestHk_FormatRecallContextByteLimit asserts the running byte cap stops
// appending once the next line would exceed hookRecallByteLimit, while keeping
// at least the first line.
func TestHk_FormatRecallContextByteLimit(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	bigTitle := strings.Repeat("A", 420) // ~420 bytes/line, so line 2 blows the 800 cap
	body := strings.Repeat("word ", 80)
	mems := []Memory{
		{ID: "first", Title: bigTitle, Source: "test", CreatedAt: now.Format(time.RFC3339), Text: body, Score: -1},
		{ID: "second", Title: bigTitle, Source: "test", CreatedAt: now.Format(time.RFC3339), Text: body, Score: -1},
		{ID: "third", Title: bigTitle, Source: "test", CreatedAt: now.Format(time.RFC3339), Text: body, Score: -1},
	}
	got := formatRecallContext(mems, 0, now)
	if len(got) > hookRecallByteLimit {
		t.Fatalf("output %d bytes exceeds cap %d", len(got), hookRecallByteLimit)
	}
	if !strings.Contains(got, "id: first") {
		t.Fatalf("first line must always fit, got:\n%s", got)
	}
	if strings.Contains(got, "id: second") {
		t.Fatalf("second line must be dropped by the byte cap, got:\n%s", got)
	}
}

// TestHk_RecallLineFallbacks exercises every fallback branch of recallLine.
func TestHk_RecallLineFallbacks(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	created := now.Format(time.RFC3339)

	// Empty text -> title becomes the snippet.
	line := recallLine(Memory{ID: "a", Title: "Only a title", Source: "manual", CreatedAt: created}, now)
	if !strings.Contains(line, "Only a title") {
		t.Fatalf("empty-text line should fall back to title, got %q", line)
	}

	// Empty text AND empty title -> no line at all.
	if got := recallLine(Memory{ID: "b", CreatedAt: created}, now); got != "" {
		t.Fatalf("text-less, title-less memory must render empty, got %q", got)
	}

	// Empty title -> id is used as the title; empty source -> "memory"; scope appended.
	line = recallLine(Memory{ID: "xyz", Source: "", Scope: "proj", CreatedAt: created, Text: "some body"}, now)
	if !strings.Contains(line, "- xyz [memory/proj,") {
		t.Fatalf("title/source fallbacks wrong, got %q", line)
	}
	if !strings.Contains(line, "id: xyz") || !strings.Contains(line, "some body") {
		t.Fatalf("recallLine missing id/snippet, got %q", line)
	}
}

// TestHk_ClipRunes covers the truncation loop and trailing-space trim.
func TestHk_ClipRunes(t *testing.T) {
	if got := clipRunes("short", 100); got != "short" {
		t.Fatalf("under-limit string must be unchanged, got %q", got)
	}
	if got := clipRunes("abcdefghij", 3); got != "abc..." {
		t.Fatalf("clipRunes truncation = %q, want %q", got, "abc...")
	}
	if got := clipRunes("ab cdef", 3); got != "ab..." {
		t.Fatalf("clipRunes must trim trailing space before the ellipsis, got %q", got)
	}
	// Multi-byte runes must be counted by rune, not byte.
	if got := clipRunes("héllo wörld", 4); got != "héll..." {
		t.Fatalf("clipRunes rune counting = %q, want %q", got, "héll...")
	}
}

// TestHk_MemoryAge covers every arm of the age formatter.
func TestHk_MemoryAge(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		created string
		want    string
	}{
		{"not-a-timestamp", "unknown"},
		{now.Add(48 * time.Hour).Format(time.RFC3339), "in the future"},
		{now.Format(time.RFC3339), "today"},
		{now.Add(-25 * time.Hour).Format(time.RFC3339), "1d"},
		{now.Add(-72 * time.Hour).Format(time.RFC3339), "3d"},
	}
	for _, c := range cases {
		if got := memoryAge(c.created, now); got != c.want {
			t.Fatalf("memoryAge(%q) = %q, want %q", c.created, got, c.want)
		}
	}
}

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
