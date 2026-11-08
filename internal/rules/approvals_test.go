package rules

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeRule(t *testing.T, vault string, r Rule) {
	t.Helper()
	path := RulePath(vault, r.ID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, Render(r), 0o600); err != nil {
		t.Fatal(err)
	}
}

func approve(t *testing.T, state string, r Rule, mode string, holdout bool) {
	t.Helper()
	if err := AppendApproval(state, Approval{RuleID: r.ID, RuleHash: Hash(r), Mode: mode, Holdout: holdout, Via: "cli-tty"}); err != nil {
		t.Fatal(err)
	}
}

func activeIDs(t *testing.T, state string) []string {
	t.Helper()
	set, present, err := LoadActive(ActivePath(state))
	if !present || err != nil {
		t.Fatalf("active.json: present=%v err=%v", present, err)
	}
	ids := []string{}
	for _, r := range set.Rules {
		ids = append(ids, r.ID+":"+r.Mode)
	}
	return ids
}

func compileIDs(t *testing.T, vault, state string) string {
	t.Helper()
	if _, err := Compile(vault, state); err != nil {
		t.Fatal(err)
	}
	return strings.Join(activeIDs(t, state), ",")
}

func TestCompile(t *testing.T) {
	vault, state := t.TempDir(), t.TempDir()
	cd := compoundCD(t)
	other := Rule{ID: "other", Message: "Other.", Spec: Spec{Version: 1, Tool: "Bash", Match: "rm -rf"}}
	writeRule(t, vault, cd)
	writeRule(t, vault, other)

	if got := compileIDs(t, vault, state); got != "" {
		t.Fatalf("no approvals: %q", got)
	}
	approve(t, state, cd, ModeShadow, false)
	approve(t, state, other, ModeWarn, false)
	approve(t, state, cd, ModeWarn, true) // last row wins
	if got := compileIDs(t, vault, state); got != "compound-cd:warn,other:warn" {
		t.Fatalf("after approvals: %q", got)
	}
	set, _, _ := LoadActive(ActivePath(state))
	if r := set.Rules[0]; !r.Holdout || r.Hash != Hash(cd) || r.Match != cd.Spec.Match || r.Message != cd.Message {
		t.Fatalf("compiled rule: %+v", r)
	}

	// off removes.
	approve(t, state, other, ModeOff, false)
	if got := compileIDs(t, vault, state); got != "compound-cd:warn" {
		t.Fatalf("after off: %q", got)
	}

	// Hash drift: the file is edited after approval, so the rule drops out.
	edited := cd
	edited.Message = "Edited by an agent."
	writeRule(t, vault, edited)
	a, _ := LoadApprovals(state)
	if _, st := Check(vault, a["compound-cd"]); st != StateChanged {
		t.Fatalf("state = %q, want %q", st, StateChanged)
	}
	if got := compileIDs(t, vault, state); got != "" {
		t.Fatalf("after drift: %q", got)
	}

	// Restoring the exact approved content brings it back.
	writeRule(t, vault, cd)
	if got := compileIDs(t, vault, state); got != "compound-cd:warn" {
		t.Fatalf("after restore: %q", got)
	}

	// A missing file drops it.
	if err := os.Remove(RulePath(vault, cd.ID)); err != nil {
		t.Fatal(err)
	}
	if _, st := Check(vault, a["compound-cd"]); st != StateMissing {
		t.Fatalf("state = %q, want %q", st, StateMissing)
	}
	if got := compileIDs(t, vault, state); got != "" {
		t.Fatalf("after remove: %q", got)
	}

	// An invalid file drops it, even when the approval hash is for that text.
	bad := cd
	bad.Spec.Match = "(?!x)cd"
	writeRule(t, vault, bad)
	approve(t, state, bad, ModeWarn, false)
	a, _ = LoadApprovals(state)
	if _, st := Check(vault, a["compound-cd"]); !strings.HasPrefix(st, "invalid: match: lookahead") {
		t.Fatalf("state = %q", st)
	}
	if got := compileIDs(t, vault, state); got != "" {
		t.Fatalf("after invalid: %q", got)
	}
	if err := os.WriteFile(RulePath(vault, cd.ID), []byte("no frontmatter"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := compileIDs(t, vault, state); got != "" {
		t.Fatalf("after garbage file: %q", got)
	}
}

func TestCompileRejectsMismatchedFileID(t *testing.T) {
	vault, state := t.TempDir(), t.TempDir()
	r := compoundCD(t)
	r.ID = "other"
	if err := os.MkdirAll(filepath.Join(vault, "rules"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A file named compound-cd.md that claims to be "other".
	if err := os.WriteFile(RulePath(vault, "compound-cd"), Render(r), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AppendApproval(state, Approval{RuleID: "compound-cd", RuleHash: Hash(r), Mode: ModeWarn}); err != nil {
		t.Fatal(err)
	}
	if got := compileIDs(t, vault, state); got != "" {
		t.Fatalf("mismatched id compiled: %q", got)
	}
}

func TestLoadApprovalsSkipsBadLines(t *testing.T) {
	vault, state := t.TempDir(), t.TempDir()
	cd := compoundCD(t)
	writeRule(t, vault, cd)
	good := `{"v":1,"ts":"2026-11-05T02:10:00.000000Z","rule_id":"compound-cd","rule_hash":"` + Hash(cd) + `","mode":"warn","holdout":true,"via":"cli-tty"}`
	lines := []string{
		good,
		"garbage",
		`{"v":1,"rule_id":"compound-cd","rule_hash":"` + Hash(cd) + `","mode":"block"}`,
		`{"v":2,"rule_id":"compound-cd","rule_hash":"` + Hash(cd) + `","mode":"off"}`,
		`{"v":1,"rule_id":"Bad ID","rule_hash":"` + Hash(cd) + `","mode":"warn"}`,
		`{"v":1,"rule_id":"compound-cd","rule_hash":"","mode":"warn"}`,
		`{"v":1,"rule_id":"compound-cd","rule_hash":"sha256:x"`,
		"",
	}
	if err := os.MkdirAll(Dir(state), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ApprovalsPath(state), []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := LoadApprovals(state)
	if err != nil || len(a) != 1 || a["compound-cd"].Mode != ModeWarn || !a["compound-cd"].Holdout {
		t.Fatalf("LoadApprovals = %+v, %v", a, err)
	}
	if got := compileIDs(t, vault, state); got != "compound-cd:warn" {
		t.Fatalf("compile: %q", got)
	}
}

func TestAppendApprovalRefusesBlock(t *testing.T) {
	state := t.TempDir()
	err := AppendApproval(state, Approval{RuleID: "compound-cd", RuleHash: "sha256:x", Mode: "block"})
	if err == nil || !strings.Contains(err.Error(), "block") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(ApprovalsPath(state)); !os.IsNotExist(err) {
		t.Fatalf("a refused approval wrote the file: %v", err)
	}
}
