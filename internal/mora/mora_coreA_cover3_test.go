package mora

// mora_coreA_cover3_test.go — coreA coverage worker, part 3. Cross-cutting error
// paths shared by nearly every command: the early `loadConfigFor(testCtx(t))` failure and the
// `flag.Parse` failure. Driving one broken input through the whole dispatch table
// exercises those guard branches in one place. Plus a few targeted branch cases.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCoreA_LoadConfigErrorPropagates makes config.toml unreadable (a directory)
// so loadConfigFor(testCtx(t)) fails, then confirms every command that loads config surfaces
// the error rather than swallowing it. Each command is given otherwise-valid args
// so control flow actually reaches the loadConfigFor(testCtx(t)) call.
func TestCoreA_LoadConfigErrorPropagates(t *testing.T) {
	withTempHome(t)
	t.Setenv("MORA_GOOGLE_CREDENTIALS", "") // never let `connect google` reach the network
	dir := configDirFor(t)
	if err := os.MkdirAll(filepath.Join(dir, "config.toml"), 0o700); err != nil {
		t.Fatal(err) // config.toml is a DIR => os.ReadFile returns a non-ErrNotExist error
	}

	cmds := [][]string{
		{"config"},
		{"write", "--title", "T", "--text", "B"},
		{"read", "some-id"},
		{"list"},
		{"search", "q"},
		{"delete", "some-id", "--yes"},
		{"context"},
		{"think", "q"},
		{"brief"},
		{"index", "rebuild"},
		{"tasks", "sync"},
		{"tasks", "add", "a-task"},
		{"tasks", "list"},
		{"tasks", "done", "a-task"},
		{"pulse"},
		{"lint"},
		{"backup"},
		{"doctor"},
		{"schedule", "list"},
		{"sources", "list"},
		{"connectors", "list"},
		{"ingest", "run"},
		{"connect", "google"},
		{"sync", "google"},
		{"reingest"},
		{"usage", "report"},
		{"disconnect", "google"},
		{"entities"},
		{"graph"},
	}
	for _, c := range cmds {
		if _, err := runErr(t, c...); err == nil {
			t.Errorf("%v: expected the loadConfig error to propagate, got nil", c)
		}
	}
}

// TestCoreA_FlagParseErrors covers the `flag.Parse` failure branch for the
// commands that use a flag.FlagSet — an unknown flag makes ContinueOnError return
// an error before any config/IO work.
func TestCoreA_FlagParseErrors(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	t.Setenv("MORA_GOOGLE_CREDENTIALS", "")

	cmds := [][]string{
		{"write", "--title", "x", "--text", "y", "--bogus"},
		{"read", "some-id", "--bogus"},
		{"list", "--bogus"},
		{"delete", "some-id", "--bogus"},
		{"context", "--bogus"},
		{"brief", "--bogus"},
		{"index", "rebuild", "--bogus"},
		{"doctor", "--bogus"},
		{"pulse", "--bogus"},
		{"tasks", "sync", "--bogus"},
		{"tasks", "add", "a-task", "--bogus"},
		{"tasks", "list", "--bogus"},
		{"connectors", "list", "--bogus"},
		{"ingest", "run", "--bogus"},
		{"connect", "google", "--bogus"},
	}
	for _, c := range cmds {
		if _, err := runErr(t, c...); err == nil {
			t.Errorf("%v: expected a flag-parse error, got nil", c)
		}
	}
}

// TestCoreA_WriteConfigEmptyExisting covers writeConfig's empty-file branch: an
// existing but empty config.toml is treated as no existing lines, and the new
// owned key is still appended.
func TestCoreA_WriteConfigEmptyExisting(t *testing.T) {
	withTempHome(t)
	dir := configDirFor(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, "config", "context", "small")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `context = "small"`) {
		t.Fatalf("empty existing config should still receive the new key:\n%s", string(b))
	}
}

// TestCoreA_ApplySetupSelectionBackfillError covers the branch where the confirmed
// backfill fails: an enabled gmail source with no token makes backfillEnabledGoogle
// return an error, which applySetupSelection must propagate.
func TestCoreA_ApplySetupSelectionBackfillError(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "gmail") // enabled, but no OAuth token exists
	t.Setenv("MORA_GOOGLE_CREDENTIALS", "")

	var out bytes.Buffer
	err := applySetupSelection(testCtx(t), cfg, []string{"gmail"}, true, &out, testStderr, strings.NewReader(""))
	if err == nil {
		t.Fatal("applySetupSelection with a failing confirmed backfill must return the error")
	}
}
