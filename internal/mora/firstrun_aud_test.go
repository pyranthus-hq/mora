package mora

import (
	"strings"
	"testing"
)

// AUD1, AUD2, AUD4 from the 2026-10-29 first-run audit: a new user's first
// searches must say when nothing matched, and `mora context <word>` must use
// the word the way `mora search <word>` does.
func TestFirstRunNoMatchAndPositionalContext(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	run(t, "write", "--scope", "project:demo", "--title", "Lunch plan", "--text", "Lunch is at noon on Fridays.")

	// AUD1: no match must not fall back to vault control files.
	out := run(t, "context", "--query", "zzzqq")
	if !strings.Contains(out, "No memory matched this query.") {
		t.Fatalf("context with no match should say so, got:\n%s", out)
	}
	if strings.Contains(out, "vault control file") {
		t.Fatalf("context with no match printed control files:\n%s", out)
	}

	// AUD2: the bare word is the query, and flags after it still apply.
	out = run(t, "context", "lunch", "--scope", "project:demo")
	if !strings.Contains(out, "Lunch plan") {
		t.Fatalf("context lunch should find the memory, got:\n%s", out)
	}
	if _, _, err := runSplit(t, "context", "lunch", "--query", "dinner"); err == nil {
		t.Fatal("context with both a word and --query should be a usage error")
	}

	// AUD4: an empty search says so on stderr and still exits 0.
	_, stderr, err := runSplit(t, "search", "zzzqq")
	if err != nil {
		t.Fatalf("empty search should exit 0: %v", err)
	}
	if !strings.Contains(stderr, `No memories match "zzzqq"`) {
		t.Fatalf("empty search should explain itself on stderr, got %q", stderr)
	}
}
