package mora

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// TestColorEnabledGate pins the gate that keeps ANSI out of machine paths.
func TestColorEnabledGate(t *testing.T) {
	t.Setenv("TERM", "xterm-256color") // ensure TERM doesn't independently disable

	// A non-file writer (pipe/redirect/test buffer) is never a TTY.
	if colorEnabled(&bytes.Buffer{}, false) {
		t.Error("buffer writer must not be color-enabled")
	}
	// The --json path is off even if stdout happens to be a terminal.
	if colorEnabled(os.Stdout, true) {
		t.Error("jsonOut must force color off")
	}
	// NO_COLOR (community standard) disables styling.
	t.Setenv("NO_COLOR", "1")
	if colorEnabled(os.Stdout, false) {
		t.Error("NO_COLOR must disable color")
	}
	t.Setenv("NO_COLOR", "")
	// TERM=dumb disables styling.
	t.Setenv("TERM", "dumb")
	if colorEnabled(os.Stdout, false) {
		t.Error("TERM=dumb must disable color")
	}
}

// TestColorDisabledForDevNull guards the /dev/null trap: it is a character
// device (so an os.ModeCharDevice check would wrongly pass) but not a terminal.
// Redirecting to it with TERM set must NOT enable ANSI.
func TestColorDisabledForDevNull(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "")
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Skipf("cannot open %s: %v", os.DevNull, err)
	}
	defer f.Close()
	if colorEnabled(f, false) {
		t.Error("/dev/null is not a terminal; color must stay off")
	}
}

// TestDigestByteCleanOnNonTTY is the load-bearing invariant: the digest path,
// when stdout is not a terminal, emits raw Markdown with zero escape codes — the
// same bytes the MCP `digest` tool returns. A regression here corrupts piped
// output and agent context, so it must stay green.
func TestDigestByteCleanOnNonTTY(t *testing.T) {
	raw := "# Mora digest — 2026-06-06 (last 24h)\n\n## Gmail (1)\n- [t] Title — snippet (id: 1)\n"

	var buf bytes.Buffer
	// mirrors cmdPulse: style with a styler gated against a non-TTY writer.
	out := styleDigestTTY(raw, newStyler(&buf, false))

	if out != raw {
		t.Fatalf("non-TTY digest must equal raw markdown\n got: %q\nwant: %q", out, raw)
	}
	if strings.ContainsRune(out, '\x1b') {
		t.Fatal("non-TTY digest contains an ANSI escape; would corrupt pipes/MCP")
	}
}
