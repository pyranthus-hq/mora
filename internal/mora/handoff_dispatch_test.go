package mora

import (
	"strings"
	"testing"
)

// TestHandoffVerbReachableThroughRunDispatch drives `mora handoff` through the
// real top-level dispatch (Run), not cmdHandoffCLI directly, so a missing
// `case "handoff"` in Run fails here.
func TestHandoffVerbReachableThroughRunDispatch(t *testing.T) {
	projectBriefSetup(t)

	out, _, err := runSplit(t, "handoff", "write", "--scope", pbScope, "--title", "Dispatch probe", "--text", "carry on from the dispatch test")
	if err != nil || !strings.Contains(out, "Dispatch probe") {
		t.Fatalf("handoff write through Run = %q, %v", out, err)
	}
	out, _, err = runSplit(t, "handoff", "list", "--scope", pbScope)
	if err != nil || !strings.Contains(out, "Dispatch probe") {
		t.Fatalf("handoff list through Run = %q, %v", out, err)
	}
	out, _, err = runSplit(t, "handoff", "show", "--scope", pbScope)
	if err != nil || !strings.Contains(out, "carry on from the dispatch test") {
		t.Fatalf("handoff show through Run = %q, %v", out, err)
	}
	// A bare `mora handoff` defaults to show.
	if out, _, err = runSplit(t, "handoff", "--scope", pbScope); err != nil || !strings.Contains(out, "Dispatch probe") {
		t.Fatalf("bare handoff through Run = %q, %v", out, err)
	}

	_, _, err = runSplit(t, "handoff", "bogus-subcommand")
	if err == nil || !strings.Contains(err.Error(), "unknown handoff command") || !strings.Contains(err.Error(), "usage: mora handoff") {
		t.Fatalf("handoff <unknown> = %v, want a usage error", err)
	}
}

func TestHelpListsHandoffVerb(t *testing.T) {
	out := run(t, "--help")
	if !strings.Contains(out, "mora handoff [show|list|write]") {
		t.Fatalf("top-level help omits the handoff verb:\n%s", out)
	}
}
