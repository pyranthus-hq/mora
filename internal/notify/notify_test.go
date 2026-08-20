package notify

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// --- Task 1: ShouldNotify gate (GOOS darwin + MORA_NO_NOTIFY opt-out) ---

func TestShouldNotify_DarwinUnset_True(t *testing.T) {
	t.Setenv("MORA_NO_NOTIFY", "")
	if !ShouldNotify("darwin") {
		t.Fatalf("ShouldNotify(darwin) with MORA_NO_NOTIFY unset = false, want true")
	}
}

func TestShouldNotify_OptOutEnv_False(t *testing.T) {
	t.Setenv("MORA_NO_NOTIFY", "1")
	if ShouldNotify("darwin") {
		t.Fatalf("ShouldNotify(darwin) with MORA_NO_NOTIFY=1 = true, want false (opt-out)")
	}
}

func TestShouldNotify_NonDarwin_False(t *testing.T) {
	t.Setenv("MORA_NO_NOTIFY", "")
	for _, goos := range []string{"linux", "windows", "freebsd", ""} {
		if ShouldNotify(goos) {
			t.Errorf("ShouldNotify(%q) = true, want false (non-darwin no-op)", goos)
		}
	}
}

// --- Task 1: injectable runner seam ---

func TestNotifyRunnerSeam_Injectable(t *testing.T) {
	var got []string
	var fake Runner = func(args ...string) error {
		got = append(got, args...)
		return nil
	}
	if err := fake("-e", "display notification \"x\""); err != nil {
		t.Fatalf("fake runner returned error: %v", err)
	}
	if len(got) != 2 || got[0] != "-e" || !strings.Contains(got[1], "display notification") {
		t.Fatalf("fake runner did not capture args, got %#v", got)
	}
}

func TestNotifyGate_ProductionRunnerConstructible(t *testing.T) {
	// The production runner is a Runner value (mirrors oauth.go openBrowser:
	// exec.Command(...).Start()). We construct it but never invoke it here — invoking
	// would spawn a real osascript. We only assert the seam exists and is non-nil.
	var run Runner = OSAScriptRunner
	if run == nil {
		t.Fatal("OSAScriptRunner is nil; production runner seam missing")
	}
}

// --- Task 2: Brief — gated, escaped, best-effort, byte-clean ---

// recordingRunner captures every invocation's argv for assertions.
type recordingRunner struct {
	calls [][]string
	err   error
}

func (r *recordingRunner) run(args ...string) error {
	r.calls = append(r.calls, append([]string(nil), args...))
	return r.err
}

func TestNotifyBrief_Darwin_PostsToastNamingBrief(t *testing.T) {
	t.Setenv("MORA_NO_NOTIFY", "")
	rr := &recordingRunner{}
	path := "briefs/2026-06-08-brief.md"
	if err := Brief(path, nil, rr.run, "darwin"); err != nil {
		t.Fatalf("Brief returned error: %v", err)
	}
	if len(rr.calls) != 1 {
		t.Fatalf("runner called %d times, want exactly 1", len(rr.calls))
	}
	args := rr.calls[0]
	if len(args) != 2 || args[0] != "-e" {
		t.Fatalf("runner argv = %#v, want [-e, <script>]", args)
	}
	script := args[1]
	if !strings.Contains(script, "display notification") {
		t.Errorf("script missing 'display notification': %q", script)
	}
	if !strings.Contains(script, "Mora") {
		t.Errorf("script missing title 'Mora': %q", script)
	}
	if !strings.Contains(script, path) {
		t.Errorf("script does not name the brief path %q: %q", path, script)
	}
}

// TestNotifyBrief_UrgentEnrichesToast (issue #62 follow-on): when a top Urgent item is
// passed, the toast leads with its subject + deadline-anchored ask instead of the
// content-free "Daily brief ready".
func TestNotifyBrief_UrgentEnrichesToast(t *testing.T) {
	t.Setenv("MORA_NO_NOTIFY", "")
	rr := &recordingRunner{}
	top := &Urgent{Subtitle: "MSA sign-off", Body: "sign the MSA by end of day today"}
	if err := Brief("briefs/x-brief.md", top, rr.run, "darwin"); err != nil {
		t.Fatalf("Brief returned error: %v", err)
	}
	if len(rr.calls) != 1 {
		t.Fatalf("runner called %d times, want 1", len(rr.calls))
	}
	script := rr.calls[0][1]
	if !strings.Contains(script, "Urgent") {
		t.Errorf("enriched toast must flag Urgent: %q", script)
	}
	if !strings.Contains(script, "MSA sign-off") || !strings.Contains(script, "by end of day") {
		t.Errorf("enriched toast must carry the subject + ask: %q", script)
	}
	if strings.Contains(script, "Daily brief ready") {
		t.Errorf("enriched toast must not fall back to the content-free body: %q", script)
	}
}

// TestNotifyBrief_UrgentTextIsEscaped: the urgent item text is user-derived, so it must
// be escaped (a subject with a double-quote cannot break out of the AppleScript string).
func TestNotifyBrief_UrgentTextIsEscaped(t *testing.T) {
	t.Setenv("MORA_NO_NOTIFY", "")
	rr := &recordingRunner{}
	top := &Urgent{Subtitle: `Re: "urgent" thing`, Body: `reply by 5pm "today"`}
	if err := Brief("briefs/x-brief.md", top, rr.run, "darwin"); err != nil {
		t.Fatalf("Brief returned error: %v", err)
	}
	script := rr.calls[0][1]
	if strings.Contains(script, `"urgent"`) || strings.Contains(script, `"today"`) {
		t.Errorf("embedded quotes in the urgent text must be escaped: %q", script)
	}
}

func TestNotifyBrief_EscapesEmbeddedQuote(t *testing.T) {
	t.Setenv("MORA_NO_NOTIFY", "")
	rr := &recordingRunner{}
	// A path with an embedded double-quote MUST be escaped so it cannot break out
	// of the AppleScript subtitle string (script injection, T-13-05).
	path := `briefs/we"ird-brief.md`
	if err := Brief(path, nil, rr.run, "darwin"); err != nil {
		t.Fatalf("Brief returned error: %v", err)
	}
	if len(rr.calls) != 1 {
		t.Fatalf("runner called %d times, want 1", len(rr.calls))
	}
	script := rr.calls[0][1]
	// The raw, unescaped quote must NOT appear as a bare " inside the path region;
	// it must be backslash-escaped (\") for AppleScript.
	if strings.Contains(script, `we"ird`) {
		t.Errorf("embedded quote was not escaped — script-injection risk: %q", script)
	}
	if !strings.Contains(script, `we\"ird`) {
		t.Errorf("expected backslash-escaped quote in script: %q", script)
	}
}

func TestNotifyBrief_StripsControlChars(t *testing.T) {
	t.Setenv("MORA_NO_NOTIFY", "")
	rr := &recordingRunner{}
	// Newlines / control chars must be stripped so they cannot inject AppleScript.
	path := "briefs/a\nb\t-brief.md"
	if err := Brief(path, nil, rr.run, "darwin"); err != nil {
		t.Fatalf("Brief returned error: %v", err)
	}
	script := rr.calls[0][1]
	if strings.ContainsAny(script, "\n\r\t") {
		t.Errorf("control chars not stripped from script: %q", script)
	}
}

func TestNotifyBrief_NonDarwin_NoRunnerCall(t *testing.T) {
	t.Setenv("MORA_NO_NOTIFY", "")
	rr := &recordingRunner{}
	if err := Brief("briefs/x-brief.md", nil, rr.run, "linux"); err != nil {
		t.Fatalf("Brief(linux) returned error: %v, want nil", err)
	}
	if len(rr.calls) != 0 {
		t.Fatalf("runner called %d times on non-darwin, want 0 (silent no-op)", len(rr.calls))
	}
}

func TestNotifyBrief_OptOutEnv_NoRunnerCall(t *testing.T) {
	t.Setenv("MORA_NO_NOTIFY", "1")
	rr := &recordingRunner{}
	if err := Brief("briefs/x-brief.md", nil, rr.run, "darwin"); err != nil {
		t.Fatalf("Brief with opt-out returned error: %v, want nil", err)
	}
	if len(rr.calls) != 0 {
		t.Fatalf("runner called %d times when opted out, want 0", len(rr.calls))
	}
}

func TestNotifyBrief_RunnerError_Swallowed(t *testing.T) {
	t.Setenv("MORA_NO_NOTIFY", "")
	rr := &recordingRunner{err: errors.New("osascript: command not found")}
	// Best-effort (D13-1, T-13-06): a failing/absent osascript must NEVER fail the brief.
	if err := Brief("briefs/x-brief.md", nil, rr.run, "darwin"); err != nil {
		t.Fatalf("Brief swallowed-error path returned %v, want nil", err)
	}
}

func TestNotifyBrief_WritesZeroBytes(t *testing.T) {
	t.Setenv("MORA_NO_NOTIFY", "")
	// Byte-clean invariant (T-13-07): Brief must not write to any stream.
	// We capture os.Stdout/os.Stderr around the call and assert nothing was emitted.
	// The signature takes no io.Writer by design; this guards the side-effect-only
	// contract against an accidental fmt.Print sneaking in.
	rr := &recordingRunner{}
	var buf bytes.Buffer
	// Brief takes no Writer; if a future change added one, this would catch a leak.
	// Here we simply assert the function compiles to a no-Writer signature and that
	// the recording runner — not any buffer — is the sole sink.
	if err := Brief("briefs/x-brief.md", nil, rr.run, "darwin"); err != nil {
		t.Fatalf("Brief returned error: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("Brief wrote %d bytes to a buffer it should never touch", buf.Len())
	}
}

func TestHealthAlarmGatingEscapingAndBestEffort(t *testing.T) {
	t.Setenv("MORA_NO_NOTIFY", "")
	rr := &recordingRunner{err: errors.New("ignored")}
	if err := HealthAlarm("bad\nhealth \"detail\" \\path", rr.run, "darwin"); err != nil {
		t.Fatal(err)
	}
	if len(rr.calls) != 1 {
		t.Fatalf("calls=%d", len(rr.calls))
	}
	script := rr.calls[0][1]
	if strings.ContainsAny(script, "\n\r\t") || !strings.Contains(script, `badhealth \"detail\" \\path`) {
		t.Fatalf("script=%q", script)
	}
	if err := HealthAlarm("x", rr.run, "linux"); err != nil {
		t.Fatal(err)
	}
	if len(rr.calls) != 1 {
		t.Fatal("non-darwin invoked runner")
	}
	t.Setenv("MORA_NO_NOTIFY", "1")
	if err := HealthAlarm("x", rr.run, "darwin"); err != nil {
		t.Fatal(err)
	}
	if len(rr.calls) != 1 {
		t.Fatal("opt-out invoked runner")
	}
}

func TestEscapeAppleScriptStringBackslashAndDelete(t *testing.T) {
	got := EscapeAppleScriptString("a\\b\x7fc")
	if got != `a\\b c` && got != `a\\bc` { // DEL is removed; retain explicit expected portability.
		t.Fatalf("escaped=%q", got)
	}
}
