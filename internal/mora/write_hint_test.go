package mora

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func writeHintRun(t *testing.T, stdin string, args ...string) (stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	if err := Run(testCtx(t), append([]string{"write"}, args...), &out, &errb, strings.NewReader(stdin)); err != nil {
		t.Fatalf("write %v: %v\n%s%s", args, err, out.String(), errb.String())
	}
	return out.String(), errb.String()
}

// A person's own plain `mora write` is saved unconfirmed; at a real terminal
// they are told once and told how to make it an owner rule. Agents, pipes and
// --json never get the line.
func TestWriteHintsThatAPlainNoteIsUnconfirmed(t *testing.T) {
	projectBriefSetup(t)
	args := []string{"--type", "decision", "--title", "Use pnpm", "--text", "Always use pnpm."}
	const hint = "saved as unconfirmed"

	reviewTerminal(t)
	if _, errText := writeHintRun(t, "", args...); !strings.Contains(errText, hint) || !strings.Contains(errText, "--mine") {
		t.Fatalf("terminal: no hint on stderr: %q", errText)
	}
	if out, _ := writeHintRun(t, "", args...); strings.Contains(out, hint) {
		t.Fatalf("the hint must ride stderr, not stdout: %q", out)
	}
	// --json: stdout stays one receipt and stderr stays quiet.
	if out, errText := writeHintRun(t, "", append(args, "--json")...); strings.Contains(errText, hint) || strings.Contains(out, hint) {
		t.Fatalf("--json: stdout=%q stderr=%q", out, errText)
	}
	// --mine asks for a typed yes and is not unconfirmed.
	if out, errText := writeHintRun(t, "yes\n", append(args, "--mine")...); strings.Contains(errText, hint) || !strings.Contains(out, "owner-confirmed") {
		t.Fatalf("--mine: stdout=%q stderr=%q", out, errText)
	}
	// A pipe is not a terminal.
	prev := rulesInteractive
	rulesInteractive = func(io.Reader) bool { return false }
	t.Cleanup(func() { rulesInteractive = prev })
	if _, errText := writeHintRun(t, "", args...); strings.Contains(errText, hint) {
		t.Fatalf("pipe: %q", errText)
	}
	// An agent session, even with a terminal.
	rulesInteractive = func(io.Reader) bool { return true }
	t.Setenv(rulesAgentEnv[0], "1")
	if _, errText := writeHintRun(t, "", args...); strings.Contains(errText, hint) {
		t.Fatalf("agent session: %q", errText)
	}
}
