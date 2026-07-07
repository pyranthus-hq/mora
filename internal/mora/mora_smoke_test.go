package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"runtime"
	"strings"
	"testing"
)

// skipOnWindows skips a test whose failure-injection mechanism is POSIX-only and
// cannot be reproduced portably on Windows (chmod-based permission denial, which
// only toggles the read-only attribute; os.Symlink, which needs
// SeCreateSymbolicLinkPrivilege; execing an extensionless #!/bin/sh stub). The
// behavior under test is correct on Windows — only the test's way of provoking
// the error is Unix-specific — so gating on GOOS keeps the assertion fully live
// on Linux AND macOS (both take the non-windows path).
func skipOnWindows(t *testing.T, reason string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("windows: " + reason)
	}
}

// assertPermUnix asserts an exact Unix permission bit set, but only off Windows.
// Windows has no Unix mode bits: os.FileInfo.Mode().Perm() is synthesized from
// ACLs and reports 0666 for any writable file (0444 for read-only), so it can
// never equal 0600/0640/0644. The production code still writes the correct mode
// (security-relevant on Unix); this only relaxes the *assertion* on Windows.
func assertPermUnix(t *testing.T, got, want os.FileMode) {
	t.Helper()
	if runtime.GOOS != "windows" && got.Perm() != want.Perm() {
		t.Fatalf("mode = %v, want %v", got.Perm(), want.Perm())
	}
}

// setTestHome points the OS home directory at dir for the duration of the test.
// It sets BOTH HOME and USERPROFILE because os.UserHomeDir — which defaultConfig
// uses to locate the vault/config/data dirs — reads USERPROFILE on Windows and
// HOME elsewhere. Setting only HOME (the original behavior) left every Windows
// test resolving the caller's REAL vault under %USERPROFILE%\vault\mora: tests
// ran against thousands of live files (slow, hit the 10m package timeout) and,
// worse, mutated the user's real vault. Setting both keeps tests hermetic on
// every OS; on Linux the extra USERPROFILE is simply ignored.
func setTestHome(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
}

// withTempHome points all home-derived dirs at a fresh temp dir on every OS.
func withTempHome(t *testing.T) {
	t.Helper()
	setTestHome(t, t.TempDir())
	// Hermeticity: a developer's exported MORA_CONFIG_DIR must not leak a real
	// config into tests that assume the temp HOME's default location.
	t.Setenv("MORA_CONFIG_DIR", "")
}

func run(t *testing.T, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	if err := Run(context.Background(), args, &out, &out, strings.NewReader("")); err != nil {
		t.Fatalf("Run(%v) error: %v\noutput:\n%s", args, err, out.String())
	}
	return out.String()
}

func TestSmokeInitWriteSearch(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	run(t, "write", "--scope", "project:wink", "--type", "decision",
		"--title", "OAuth auth path", "--text", "Use OAuth 2.0 for Wink API auth")

	out := run(t, "search", "OAuth", "--scope", "project:wink", "--json")
	var got []Memory
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("search json: %v\n%s", err, out)
	}
	if len(got) != 1 || got[0].Title != "OAuth auth path" {
		t.Fatalf("expected 1 hit titled 'OAuth auth path', got %+v", got)
	}
	if got[0].Scope != "project:wink" || got[0].Type != "decision" {
		t.Fatalf("expected scoped decision memory, got %+v", got[0])
	}
	if !strings.Contains(got[0].Text, "OAuth 2.0") {
		t.Fatalf("expected search result text to include written body, got %+v", got[0])
	}
}

func TestSmokeMCPSearchRoundtrip(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	run(t, "write", "--scope", "global", "--title", "Alpha", "--text", "alpha body")

	text, isErr := mcpToolText(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_memory","arguments":{"query":"alpha"}}}`)
	if isErr {
		t.Fatalf("search_memory unexpectedly isError; text=%s", text)
	}
	var payload struct {
		Results []Memory `json:"results"`
	}
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("decode CallToolResult text: %v\n%s", err, text)
	}
	hits := payload.Results
	if len(hits) != 1 || hits[0].Title != "Alpha" {
		t.Fatalf("expected MCP search to return Alpha, got: %s", text)
	}
	if hits[0].Scope != "global" || hits[0].Text != "alpha body" {
		t.Fatalf("expected MCP search to return written memory, got %+v", hits[0])
	}
}

func TestDoctorReportsInjectedWindowsPlatform(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	origGOOS := runtimeGOOS
	t.Cleanup(func() { runtimeGOOS = origGOOS })
	runtimeGOOS = func() string { return "windows" }

	var js bytes.Buffer
	if err := cmdDoctor(context.Background(), []string{"--json"}, &js); err != nil {
		t.Fatal(err)
	}
	var rep doctorReport
	if err := json.Unmarshal(js.Bytes(), &rep); err != nil {
		t.Fatalf("doctor --json is not valid JSON: %v\n%s", err, js.String())
	}
	if rep.Platform != "windows" {
		t.Fatalf("doctor platform = %q, want windows", rep.Platform)
	}

	var text bytes.Buffer
	if err := cmdDoctor(context.Background(), nil, &text); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text.String(), "skipping chat.db checks on windows") {
		t.Fatalf("doctor should report iMessage as macOS-only on windows; got:\n%s", text.String())
	}
}
