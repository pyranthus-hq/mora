package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"testing"
	"time"
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

// setTestHome points Mora's home-derived dirs at dir for the duration of the
// test by injecting a home root into the caller's test context — the exact
// layout os.UserHomeDir-driven resolution used to produce from HOME/USERPROFILE
// (which this helper set process-globally before #319; see testctx_test.go).
// The empty string remains valid: it models a missing HOME exactly like the old
// t.Setenv("HOME", "") did.
func setTestHome(t *testing.T, dir string) {
	t.Helper()
	e := lookupTestEnv(t)
	if e == nil {
		e = &testEnv{}
		bindTestEnv(t, e)
	}
	cp := *e
	cp.home = dir
	cp.configRoot = ""
	bindTestEnv(t, &cp)
}

// setAuthoredReconcileRunnerForTest pins the async authored-write reconciler on
// the caller's injected environment instead of swapping a package global, so
// concurrent tests never share (or race each other's cleanup of) this seam.
func setAuthoredReconcileRunnerForTest(t *testing.T, runner func(context.Context, Config) error) {
	t.Helper()
	e := lookupTestEnv(t)
	if e == nil {
		e = &testEnv{}
		bindTestEnv(t, e)
	}
	e.reconciler = runner
}

// withTempHome points all home-derived dirs at a fresh temp dir without
// touching process environment: the root rides the caller's test context, so
// tests using it MAY run in parallel (#319). Hermeticity guarantees carry over:
// a developer's exported MORA_CONFIG_DIR / MORA_VAULT cannot leak a real config
// or vault in, because an injected root makes config resolution skip the
// process environment entirely (configstore.LoadFrom).
func withTempHome(t *testing.T) {
	t.Helper()
	// Async MCP reconciliation is a process-lifetime production worker. Hermetic
	// temp-home tests may tear their StateDir down immediately after a call, so keep
	// it inert by default and opt in with a local seam where its scheduling contract
	// is under test. The seam rides the per-test env, so this stays true per test
	// even under parallelism.
	env := &testEnv{home: t.TempDir(), reconciler: func(context.Context, Config) error { return nil }}
	bindTestEnv(t, env)
	pinOperationClockForTest(t, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
}

func run(t *testing.T, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	if err := Run(testCtx(t), args, &out, &out, strings.NewReader("")); err != nil {
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
	// Plan 01-07: `search --json` carries its array under `memories`.
	got, err := decodeMemoriesJSON(t, out)
	if err != nil {
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
	if err := cmdDoctor(testCtx(t), []string{"--json"}, &js, testStderr); err != nil {
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
	if err := cmdDoctor(testCtx(t), nil, &text, testStderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text.String(), "skipping chat.db checks on windows") {
		t.Fatalf("doctor should report iMessage as macOS-only on windows; got:\n%s", text.String())
	}
}
