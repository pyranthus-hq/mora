package mora

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/rules"
)

// assertPreToolFailOpen runs the hook and requires the fail-open contract:
// nil error, empty stderr, and stdout either empty or one whole JSON line.
func assertPreToolFailOpen(t *testing.T, input string) string {
	t.Helper()
	var out, errb bytes.Buffer
	if err := cmdHook(testCtx(t), []string{"pre-tool-use"}, &out, &errb, strings.NewReader(input)); err != nil {
		t.Fatalf("hook returned %v", err)
	}
	if errb.Len() != 0 {
		t.Fatalf("stderr = %q", errb.String())
	}
	if s := out.String(); s != "" {
		if !strings.HasSuffix(s, "\n") || strings.Count(s, "\n") != 1 {
			t.Fatalf("partial stdout %q", s)
		}
		decodeHookOutput(t, s)
	}
	return out.String()
}

func TestHk_PreToolFailOpen(t *testing.T) {
	big := `{"session_id":"s1","tool_name":"Bash","tool_input":{"command":"cd /tmp && ` + strings.Repeat("x", 2<<20) + `"}}`
	for _, tc := range []struct{ name, input string }{
		{"garbage", "not json at all"},
		{"empty object", "{}"},
		{"empty", ""},
		{"json array", `[1,2]`},
		{"two MiB", big},
		{"non-string command", `{"session_id":"s1","tool_name":"Bash","tool_input":{"command":7}}`},
	} {
		subRun(t, tc.name, func(t *testing.T) {
			seedPreToolActive(t, nil)
			if got := assertPreToolFailOpen(t, tc.input); got != "" {
				t.Fatalf("printed %q", got)
			}
		})
	}
}

func TestHk_PreToolConfigError(t *testing.T) {
	cfg := seedPreToolActive(t, nil)
	prev := hookPreToolLoadConfig
	hookPreToolLoadConfig = func(context.Context) (Config, error) { return Config{}, errors.New("config boom") }
	t.Cleanup(func() { hookPreToolLoadConfig = prev })
	if got := assertPreToolFailOpen(t, preToolInputS1); got != "" {
		t.Fatalf("printed %q", got)
	}
	assertNoPreToolLog(t, cfg)
}

func TestHk_PreToolCorruptActiveWritesNote(t *testing.T) {
	cfg := seedPreToolActive(t, nil)
	if err := os.WriteFile(rules.ActivePath(cfg.StateDir), []byte(`{"schema":"mora.rules.active.v1","rules":[`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := assertPreToolFailOpen(t, preToolInputS1); got != "" {
		t.Fatalf("printed %q", got)
	}
	row := lastPreToolRow(t, cfg)
	if row.Note != "active_invalid" || len(row.Matched) != 0 {
		t.Fatalf("row = %+v", row)
	}
}

func TestHk_PreToolInvalidRuleNextToValid(t *testing.T) {
	cfg := seedPreToolActive(t, func(s *rules.ActiveSet) {
		bad := s.Rules[0]
		bad.ID, bad.Match = "a-bad", `(?!x)cd`
		block := s.Rules[0]
		block.ID, block.Mode = "b-block", "block"
		s.Rules = append([]rules.ActiveRule{bad, block}, s.Rules...)
	})
	got := decodeHookOutput(t, assertPreToolFailOpen(t, preToolInputS1))
	if !strings.HasPrefix(got.HookSpecificOutput.AdditionalContext, "Use one command per Bash call.") {
		t.Fatalf("valid rule should still warn, got %+v", got)
	}
	row := lastPreToolRow(t, cfg)
	if row.Note != "rule_invalid:a-bad,rule_invalid:b-block" || strings.Join(row.Warned, ",") != "compound-cd" || len(row.Rules) != 1 {
		t.Fatalf("row = %+v", row)
	}
}

func TestHk_PreToolLoadPanics(t *testing.T) {
	seedPreToolActive(t, nil)
	prev := hookPreToolLoad
	hookPreToolLoad = func(string) (rules.ActiveSet, bool, error) { panic("x") }
	t.Cleanup(func() { hookPreToolLoad = prev })
	if got := assertPreToolFailOpen(t, preToolInputS1); got != "" {
		t.Fatalf("printed %q", got)
	}
}

func TestHk_PreToolBudgetSlowLoad(t *testing.T) {
	cfg := seedPreToolActive(t, nil)
	prevLoad, prevBudget := hookPreToolLoad, hookPreToolBudget
	release := make(chan struct{})
	hookPreToolLoad = func(string) (rules.ActiveSet, bool, error) {
		<-release
		return rules.ActiveSet{}, false, nil
	}
	hookPreToolBudget = time.Millisecond
	t.Cleanup(func() { hookPreToolLoad, hookPreToolBudget = prevLoad, prevBudget })
	// Registered last, so it runs first: the stuck worker finishes before the
	// vars are restored. It captured its dependencies when it started anyway.
	t.Cleanup(func() { close(release) })
	start := time.Now()
	if got := assertPreToolFailOpen(t, preToolInputS1); got != "" {
		t.Fatalf("printed %q", got)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("budget not enforced: %v", elapsed)
	}
	assertNoPreToolLog(t, cfg)
}
