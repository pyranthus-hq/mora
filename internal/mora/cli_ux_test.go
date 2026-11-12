package mora

import (
	"strings"
	"testing"
)

func TestReadPrintsNoteBodyBelowHeader(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	out := run(t, "write", "--scope", "global", "--type", "decision", "--title", "Use Postgres 16", "--text", "We use Postgres 16 for local dev.")
	id := strings.Fields(out)[0]
	got := run(t, "read", id)
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) < 3 || !strings.HasPrefix(lines[0], id) || !strings.Contains(lines[0], "Use Postgres 16") {
		t.Fatalf("header line missing:\n%s", got)
	}
	if !strings.Contains(got, "We use Postgres 16 for local dev.") {
		t.Fatalf("mora read must print the note body:\n%s", got)
	}
	if strings.Contains(run(t, "read", id, "--json"), "\n\nWe use") {
		t.Fatal("--json must stay JSON only")
	}
}

func TestReadCutsHugeBodyForHumans(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	pbSeed(t, cfg, "mem_big", "global", "note", "Big chat", strings.Repeat("word ", 5000), 2)
	got := run(t, "read", "mem_big")
	if len(got) > readBodyRunes+400 || !strings.Contains(got, "text cut at") {
		t.Fatalf("a huge note must be cut with a pointer to --json (len %d)", len(got))
	}
}

func TestGivenShowsTitlesResolvedFromVaultNotStoredInLog(t *testing.T) {
	cfg := projectBriefSetup(t)
	pbSeed(t, cfg, "mem_a", "global", "decision", "Always deploy via staging", "x", 2)
	pbSeed(t, cfg, "mem_b", "global", "decision", "Evil\x1b[31m title\nwith newline", "x", 3)
	if err := appendGiven(cfg, givenLine{Surface: "hook-recall", Kind: "recall", Caller: "claude-code"}, []string{"mem_a", "mem_b", "mem_missing"}, 3); err != nil {
		t.Fatal(err)
	}
	if raw := readGivenRaw(t, cfg); strings.Contains(raw, "staging") {
		t.Fatalf("titles must never be written to the given log: %s", raw)
	}
	got := run(t, "given")
	if !strings.Contains(got, "    mem_a  Always deploy via staging") {
		t.Errorf("given output lacks the title of mem_a:\n%q", got)
	}
	if !strings.Contains(got, "    mem_b  Evil title with newline") && !strings.Contains(got, "    mem_b  Evil [31m title with newline") {
		t.Errorf("given output lacks a flattened title for mem_b:\n%q", got)
	}
	if strings.Contains(got, "\x1b") {
		t.Fatalf("a terminal escape from a note title reached the terminal: %q", got)
	}
	if strings.Contains(got, "    mem_missing") {
		t.Fatalf("an unresolved id must not get a title line:\n%s", got)
	}
	if strings.Contains(run(t, "given", "--json"), "staging") {
		t.Fatal("--json output must stay the stored lines only")
	}
}

func TestWriteDefaultsToBoundProjectScope(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	dir := pbBind(t)
	t.Chdir(dir)
	scopeOf := func(out string) string {
		// run merges stderr, which carries the "saving to" notice, into out.
		for _, line := range strings.Split(out, "\n") {
			if f := strings.Split(line, "\t"); len(f) >= 3 {
				return f[1]
			}
		}
		t.Fatalf("unexpected write output %q", out)
		return ""
	}
	if got := scopeOf(run(t, "write", "--type", "decision", "--title", "Use Vitest", "--text", "Run unit tests with vitest.")); got != pbScope {
		t.Fatalf("a write inside a bound folder must default to %s, got %q", pbScope, got)
	}
	if got := scopeOf(run(t, "write", "--scope", "global", "--type", "decision", "--title", "Explicit global", "--text", "Stays global.")); got != "global" {
		t.Fatalf("an explicit --scope must win, got %q", got)
	}
	t.Chdir(t.TempDir())
	if got := scopeOf(run(t, "write", "--type", "decision", "--title", "Unbound", "--text", "Outside any project.")); got != "global" {
		t.Fatalf("outside a bound folder the default stays global, got %q", got)
	}
}

func TestUsageIsGroupedAndNamesNewCommands(t *testing.T) {
	var out strings.Builder
	printUsage(&out)
	u := out.String()
	for _, want := range []string{
		"GET STARTED AND CHECK HEALTH:", "SAVE, FIND AND READ NOTES:", "REVIEW WHAT AGENTS SAVED AND RECEIVED:",
		"mora status", "mora status --web", "mora project bind", "mora given", "mora review", "mora hook install --client antigravity",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("usage lacks %q", want)
		}
	}
	if strings.Index(u, "mora init") > strings.Index(u, "mora write") {
		t.Error("getting started must come before the rest")
	}
}
