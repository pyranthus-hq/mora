package rules

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func mustMatcher(t *testing.T, id string, s Spec) *Matcher {
	t.Helper()
	m, err := NewMatcher(id, s)
	if err != nil {
		t.Fatalf("NewMatcher(%s): %v", id, err)
	}
	return m
}

func TestMatchCompoundCD(t *testing.T) {
	m := mustMatcher(t, "compound-cd", compoundCD(t).Spec)
	for _, cmd := range []string{"cd /tmp && ls", "cd /tmp; ls", "  cd a && b", "cd /tmp && make"} {
		if !m.Hit(Action{Command: cmd}) {
			t.Errorf("want hit: %q", cmd)
		}
	}
	for _, cmd := range []string{"ls && cd x", "echo cd x && y", "cd /tmp"} {
		if m.Hit(Action{Command: cmd}) {
			t.Errorf("want miss: %q", cmd)
		}
	}
}

func TestMatchExclude(t *testing.T) {
	sigs := loadPrototype(t)
	m := mustMatcher(t, "memory-data-folder", sigs["memory-data-folder"])
	if !m.Hit(Action{Command: "rg foo ~/vault/mora/memories"}) {
		t.Error("plain read should hit")
	}
	for _, cmd := range []string{
		"find ~ -not -path '~/vault/mora/*' -name x",
		"grep -r foo ~ --exclude-dir=~/vault/mora",
		"rg foo ~ --glob='!$HOME/vault/mora'",
	} {
		if m.Hit(Action{Command: cmd}) {
			t.Errorf("excluded text should not hit: %q", cmd)
		}
	}
	// The exclusion removes only its own text; a real read later still hits.
	if !m.Hit(Action{Command: "find ~ -path ~/vault/mora -prune -o -name x -print; cat ~/vault/mora/y.md"}) {
		t.Error("read after a prune should hit")
	}
}

func TestMatchUnless(t *testing.T) {
	m := mustMatcher(t, "cli", Spec{Version: 1, Tool: "Bash", Match: `(?:^|[;&|]\s*)mora\s+(?:read|search)\b`, Unless: `--json\b`})
	cases := map[string]bool{
		"mora read x":                       true,
		"mora read x --json":                false,
		"mora read x; mora search y --json": true,
		"mora read x --json; mora search y": true,
		"mora search y; echo --json":        true,
		"mora read x --jsonx":               true,
	}
	for cmd, want := range cases {
		if got := m.Hit(Action{Command: cmd}); got != want {
			t.Errorf("Hit(%q) = %v, want %v", cmd, got, want)
		}
	}
}

func TestMatchSubagentContext(t *testing.T) {
	m := mustMatcher(t, "nested", Spec{Version: 1, Tool: "Bash", Match: `codex\s+exec`, Context: ContextSubagent})
	if m.Hit(Action{Command: "codex exec x"}) {
		t.Error("subagent rule fired on the main thread")
	}
	if !m.Hit(Action{Command: "codex exec x", AgentID: "a1"}) {
		t.Error("subagent rule missed in a subagent")
	}
}

func TestMatchReturnsIDOrder(t *testing.T) {
	ls := Spec{Version: 1, Tool: "Bash", Match: `ls`}
	rules := []*Matcher{mustMatcher(t, "zeta", ls), mustMatcher(t, "alpha", ls), mustMatcher(t, "mid", Spec{Version: 1, Tool: "Bash", Match: `nope`})}
	got := Match(rules, Action{Command: "ls"})
	if !reflect.DeepEqual(got, []string{"alpha", "zeta"}) {
		t.Fatalf("Match = %v", got)
	}
	if got := Match(nil, Action{Command: "ls"}); got == nil || len(got) != 0 {
		t.Fatalf("Match(nil) = %#v, want empty non-nil", got)
	}
}

// TestMatchUnlessLongCommand pins the cap on unless checks. Every match tail
// below holds --no-verify, so without the cap each of the ~50k matches scans
// the rest of the 200 KB command.
func TestMatchUnlessLongCommand(t *testing.T) {
	m := mustMatcher(t, "verify", Spec{Version: 1, Tool: "Bash", Match: `\bgit\b`, Unless: `--no-verify`})
	cmd := "git commit" + strings.Repeat(" git", 50000) + " --no-verify"
	start := time.Now()
	if m.Hit(Action{Command: cmd}) {
		t.Error("every tail holds --no-verify, want miss")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Hit took %v on a 200 KB command", d)
	}
}
