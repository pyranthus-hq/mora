package integrations

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// wireSeams is fsSeams with a controlled environment: a wire test must never
// see the developer's real CODEX_HOME or Orca data dir, or it would plan
// (and apply) changes outside the temp home.
func wireSeams(t *testing.T, home string, env map[string]string) Seams {
	t.Helper()
	s := fsSeams(t, home)
	s.Getenv = func(k string) string { return env[k] }
	return s
}

func mkdirs(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// snapshot records every regular file under root, so a test can prove that a
// dry run wrote nothing and that unwire restored the tree byte for byte.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			out[path] = string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sameSnapshot(t *testing.T, want, got map[string]string) {
	t.Helper()
	var keys []string
	for k := range want {
		keys = append(keys, k)
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		w, wok := want[k]
		g, gok := got[k]
		switch {
		case !wok:
			t.Errorf("unexpected new file %s:\n%s", k, g)
		case !gok:
			t.Errorf("file %s was removed", k)
		case w != g:
			t.Errorf("file %s changed:\n--- want\n%s\n--- got\n%s", k, w, g)
		}
	}
}

// fullHome lays out every agent wire knows about: Claude Code, the default
// Codex home, one Orca-managed Codex home, and Pi.
func fullHome(t *testing.T) (home, orcaCodex string) {
	t.Helper()
	home = t.TempDir()
	orcaCodex = filepath.Join(home, ".config", "orca", "codex-accounts", "acct-1", "home")
	mkdirs(t, filepath.Join(home, ".claude"), filepath.Join(home, ".codex"), orcaCodex, filepath.Join(home, ".pi", "agent"))
	return home, orcaCodex
}

func changeFor(t *testing.T, p Plan, path string) FileChange {
	t.Helper()
	for _, c := range p.Changes {
		if c.Path == path {
			return c
		}
	}
	t.Fatalf("no planned change for %s; plan: %+v", path, p.Changes)
	return FileChange{}
}

func TestWireDryRunPlansEveryDetectedAgentAndWritesNothing(t *testing.T) {
	home, orcaCodex := fullHome(t)
	seams := wireSeams(t, home, nil)
	before := snapshot(t, home)

	plan, err := PlanWire(seams, WireOptions{Binary: "/opt/mora/bin/mora"})
	if err != nil {
		t.Fatal(err)
	}
	sameSnapshot(t, before, snapshot(t, home))

	want := map[string]string{
		filepath.Join(home, ".claude.json"):              "claude",
		filepath.Join(home, ".claude", "CLAUDE.md"):      "claude",
		filepath.Join(home, ".codex", "config.toml"):     "codex",
		filepath.Join(home, ".codex", "AGENTS.md"):       "codex",
		filepath.Join(orcaCodex, "config.toml"):          "codex",
		filepath.Join(orcaCodex, "AGENTS.md"):            "codex",
		filepath.Join(home, ".pi", "agent", "AGENTS.md"): "pi",
	}
	if len(plan.Changes) != len(want) {
		t.Fatalf("want %d planned files, got %+v", len(want), plan.Changes)
	}
	for path, agent := range want {
		c := changeFor(t, plan, path)
		if c.Agent != agent || c.Action != ActionCreate {
			t.Errorf("%s: want %s create, got %+v", path, agent, c)
		}
		if !strings.Contains(c.Diff, "+") {
			t.Errorf("%s: a planned create must show its diff, got %q", path, c.Diff)
		}
	}
	if plan.Pending() != len(want) {
		t.Fatalf("pending = %d, want %d", plan.Pending(), len(want))
	}
}

func TestWireOnEmptyHomePlansNothing(t *testing.T) {
	home := t.TempDir()
	plan, err := PlanWire(wireSeams(t, home, nil), WireOptions{Binary: "/opt/mora"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Changes) != 0 || plan.Pending() != 0 {
		t.Fatalf("no agent detected, want no plan; got %+v", plan.Changes)
	}
}

func TestWireIsIdempotent(t *testing.T) {
	home, _ := fullHome(t)
	seams := wireSeams(t, home, nil)
	plan, err := PlanWire(seams, WireOptions{Binary: "/opt/mora"})
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(seams, plan); err != nil {
		t.Fatal(err)
	}
	afterFirst := snapshot(t, home)

	again, err := PlanWire(seams, WireOptions{Binary: "/opt/mora"})
	if err != nil {
		t.Fatal(err)
	}
	if again.Pending() != 0 {
		t.Fatalf("second wire must change nothing; got %+v", again.Changes)
	}
	for _, c := range again.Changes {
		if c.Action != ActionUnchanged || c.Diff != "" {
			t.Fatalf("second wire: want unchanged with no diff, got %+v", c)
		}
	}
	if err := Apply(seams, again); err != nil {
		t.Fatal(err)
	}
	sameSnapshot(t, afterFirst, snapshot(t, home))
}

func TestWireCodexApprovesReadOnlyToolsOnly(t *testing.T) {
	home, orcaCodex := fullHome(t)
	seams := wireSeams(t, home, nil)
	plan, err := PlanWire(seams, WireOptions{Binary: "/opt/mora"})
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(seams, plan); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(home, ".codex", "config.toml"), filepath.Join(orcaCodex, "config.toml")} {
		body := readFile(t, path)
		if !strings.Contains(body, "[mcp_servers.mora]\ncommand = \"/opt/mora\"\nargs = [\"mcp\", \"serve\"]") {
			t.Fatalf("%s: mora server not registered:\n%s", path, body)
		}
		for _, tool := range CodexReadOnlyTools {
			if !strings.Contains(body, "[mcp_servers.mora.tools."+tool+"]\napproval_mode = \"approve\"") {
				t.Errorf("%s: read tool %s not pre-approved:\n%s", path, tool, body)
			}
		}
		for _, tool := range []string{"write_memory", "delete_memory"} {
			if strings.Contains(body, "tools."+tool) {
				t.Errorf("%s: mutating tool %s must keep Codex's own approval:\n%s", path, tool, body)
			}
		}
	}
}

func TestWireKeepsAnExistingRegistrationAndUserApprovals(t *testing.T) {
	home := t.TempDir()
	mkdirs(t, filepath.Join(home, ".codex"))
	cfg := filepath.Join(home, ".codex", "config.toml")
	writeFixture(t, cfg, `model = "synthetic"

[mcp_servers.mora]
command = "/home/u/.local/bin/mora"
args = ["mcp", "serve"]

[mcp_servers.mora.tools.search_memory]
approval_mode = "prompt"

[features]
flag = true
`)
	seams := wireSeams(t, home, nil)
	plan, err := PlanWire(seams, WireOptions{Binary: "/somewhere/else/mora"})
	if err != nil {
		t.Fatal(err)
	}
	c := changeFor(t, plan, cfg)
	if c.Action != ActionUpdate {
		t.Fatalf("want update (approvals only), got %+v", c)
	}
	if err := Apply(seams, plan); err != nil {
		t.Fatal(err)
	}
	body := readFile(t, cfg)
	if !strings.Contains(body, `command = "/home/u/.local/bin/mora"`) || strings.Contains(body, "/somewhere/else") {
		t.Fatalf("wire must not repoint an existing registration:\n%s", body)
	}
	if !strings.Contains(body, "[mcp_servers.mora.tools.search_memory]\napproval_mode = \"prompt\"") {
		t.Fatalf("wire must keep the user's own approval choice:\n%s", body)
	}
	if strings.Count(body, "[mcp_servers.mora.tools.search_memory]") != 1 {
		t.Fatalf("duplicate tool table:\n%s", body)
	}
	if !strings.HasPrefix(body, "model = \"synthetic\"\n") || !strings.HasSuffix(body, "[features]\nflag = true\n") {
		t.Fatalf("unrelated config moved:\n%s", body)
	}
	// The added tables sit inside the mora family, before [features].
	if strings.Index(body, "tools.read_memory") > strings.Index(body, "[features]") {
		t.Fatalf("approvals must stay with the mora table:\n%s", body)
	}
}

func TestWireRefusesAToolsTableItCannotExtendSafely(t *testing.T) {
	home := t.TempDir()
	mkdirs(t, filepath.Join(home, ".codex"))
	writeFixture(t, filepath.Join(home, ".codex", "config.toml"), "[mcp_servers.mora]\ncommand = \"/m\"\nargs = [\"mcp\", \"serve\"]\ntools = { search_memory = { approval_mode = \"approve\" } }\n")
	if _, err := PlanWire(wireSeams(t, home, nil), WireOptions{Binary: "/m"}); err == nil {
		t.Fatal("an inline tools table must be an explicit error, not a duplicate-key config")
	}
}

func TestWireHonorsCodexHomeAndDedupesIt(t *testing.T) {
	home, orcaCodex := fullHome(t)
	custom := filepath.Join(home, "codex-custom")
	mkdirs(t, custom)
	env := map[string]string{"CODEX_HOME": orcaCodex}
	plan, err := PlanWire(wireSeams(t, home, env), WireOptions{Binary: "/opt/mora"})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, c := range plan.Changes {
		if c.Path == filepath.Join(orcaCodex, "config.toml") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("CODEX_HOME equal to an Orca home must be planned once, got %d", n)
	}

	env = map[string]string{"CODEX_HOME": custom}
	plan, err = PlanWire(wireSeams(t, home, env), WireOptions{Binary: "/opt/mora"})
	if err != nil {
		t.Fatal(err)
	}
	changeFor(t, plan, filepath.Join(custom, "config.toml"))
	changeFor(t, plan, filepath.Join(custom, "AGENTS.md"))
}

// Env-derived homes count only inside the home being wired. A run against a
// temp HOME (every test, and any sandboxed run) must never reach the
// developer's real Codex or Orca home through an inherited variable.
func TestWireIgnoresEnvHomesOutsideHome(t *testing.T) {
	home := t.TempDir()
	outside := t.TempDir()
	codex := filepath.Join(outside, "codex")
	orca := filepath.Join(outside, "orca", "codex-accounts", "x", "home")
	mkdirs(t, codex, orca)
	env := map[string]string{"CODEX_HOME": codex, "ORCA_USER_DATA_PATH": filepath.Join(outside, "orca")}
	plan, err := PlanWire(wireSeams(t, home, env), WireOptions{Binary: "/opt/mora"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Changes) != 0 {
		t.Fatalf("homes outside %s must be ignored, got %+v", home, plan.Changes)
	}
	un, err := PlanUnwire(wireSeams(t, home, env), nil, true)
	if err != nil || len(un.Changes) != 0 {
		t.Fatalf("unwire must ignore them too: %+v %v", un.Changes, err)
	}
}

func TestWireFindsOrcaHomesUnderOrcaUserDataPath(t *testing.T) {
	home := t.TempDir()
	data := filepath.Join(home, "orca-data")
	managed := filepath.Join(data, "codex-accounts", "x", "home")
	mkdirs(t, managed)
	plan, err := PlanWire(wireSeams(t, home, map[string]string{"ORCA_USER_DATA_PATH": data}), WireOptions{Binary: "/opt/mora"})
	if err != nil {
		t.Fatal(err)
	}
	changeFor(t, plan, filepath.Join(managed, "config.toml"))
}

func TestWireInstructionBlocks(t *testing.T) {
	home, _ := fullHome(t)
	claudeMD := filepath.Join(home, ".claude", "CLAUDE.md")
	userText := "# My rules\n\nBe brief.\n"
	writeFixture(t, claudeMD, userText)
	seams := wireSeams(t, home, nil)
	plan, err := PlanWire(seams, WireOptions{Binary: "/opt/mora"})
	if err != nil {
		t.Fatal(err)
	}
	if c := changeFor(t, plan, claudeMD); c.Action != ActionUpdate || strings.Contains(c.Diff, "Be brief") {
		t.Fatalf("CLAUDE.md: want an update whose diff shows only the Mora block, got %+v", c)
	}
	if err := Apply(seams, plan); err != nil {
		t.Fatal(err)
	}
	body := readFile(t, claudeMD)
	if !strings.HasPrefix(body, userText+"\n"+BlockBegin) || !strings.HasSuffix(body, BlockEnd+"\n") {
		t.Fatalf("block must be appended after the user's text:\n%s", body)
	}
	for _, want := range []string{"search_memory", "read_memory", "write_memory", "Never read Mora's files directly", "not the shell", "share", "Search first"} {
		if !strings.Contains(body, want) {
			t.Errorf("MCP block missing %q:\n%s", want, body)
		}
	}
	pi := readFile(t, filepath.Join(home, ".pi", "agent", "AGENTS.md"))
	for _, want := range []string{`mora search "`, "--json", "mora read <id> --json", "mora write", "Never read Mora's files directly", "Search first"} {
		if !strings.Contains(pi, want) {
			t.Errorf("CLI block missing %q:\n%s", want, pi)
		}
	}
	if strings.Contains(pi, "search_memory") {
		t.Errorf("Pi has no MCP; its block must not name MCP tools:\n%s", pi)
	}
}

func TestWireRefreshesAStaleBlockInPlace(t *testing.T) {
	home := t.TempDir()
	mkdirs(t, filepath.Join(home, ".pi", "agent"))
	path := filepath.Join(home, ".pi", "agent", "AGENTS.md")
	writeFixture(t, path, "top\n\n"+BlockBegin+"\nold advice\n"+BlockEnd+"\n\nbottom\n")
	seams := wireSeams(t, home, nil)
	plan, err := PlanWire(seams, WireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	c := changeFor(t, plan, path)
	if c.Action != ActionUpdate || !strings.Contains(c.Diff, "-old advice") {
		t.Fatalf("want an update that removes the stale line, got %+v", c)
	}
	if err := Apply(seams, plan); err != nil {
		t.Fatal(err)
	}
	body := readFile(t, path)
	if !strings.HasPrefix(body, "top\n\n"+BlockBegin+"\n") || !strings.HasSuffix(body, BlockEnd+"\n\nbottom\n") || strings.Contains(body, "old advice") {
		t.Fatalf("refresh must replace only the block:\n%s", body)
	}
	if strings.Count(body, BlockBegin) != 1 {
		t.Fatalf("duplicate block:\n%s", body)
	}
}

func TestWireRefusesBrokenMarkers(t *testing.T) {
	for name, body := range map[string]string{
		"begin only": "x\n" + BlockBegin + "\n",
		"end only":   BlockEnd + "\n",
		"reversed":   BlockEnd + "\n" + BlockBegin + "\n",
		"twice":      BlockBegin + "\n" + BlockEnd + "\n" + BlockBegin + "\n" + BlockEnd + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			mkdirs(t, filepath.Join(home, ".pi", "agent"))
			writeFixture(t, filepath.Join(home, ".pi", "agent", "AGENTS.md"), body)
			if _, err := PlanWire(wireSeams(t, home, nil), WireOptions{}); err == nil {
				t.Fatal("broken markers must be an explicit error")
			}
			if _, err := PlanUnwire(wireSeams(t, home, nil), nil, false); err == nil {
				t.Fatal("broken markers must be an explicit error on unwire too")
			}
		})
	}
}

func TestUnwireReversesWireByteForByte(t *testing.T) {
	home, orcaCodex := fullHome(t)
	writeFixture(t, filepath.Join(home, ".claude", "CLAUDE.md"), "# Mine\n\nKeep this.\n")
	writeFixture(t, filepath.Join(home, ".codex", "config.toml"), "model = \"synthetic\"\n\n[mcp_servers.other]\ncommand = \"/usr/local/bin/other\"\nargs = [\"serve\"]\n")
	writeFixture(t, filepath.Join(home, ".claude.json"), "{\n  \"mcpServers\": {\n    \"other\": {\n      \"command\": \"/o\"\n    }\n  },\n  \"theme\": \"dark\"\n}\n")
	writeFixture(t, filepath.Join(orcaCodex, "AGENTS.md"), "# Orca codex rules\n")
	seams := wireSeams(t, home, nil)
	before := snapshot(t, home)

	plan, err := PlanWire(seams, WireOptions{Binary: "/opt/mora"})
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(seams, plan); err != nil {
		t.Fatal(err)
	}
	un, err := PlanUnwire(seams, plan.Owned(), false)
	if err != nil {
		t.Fatal(err)
	}
	if un.Pending() == 0 {
		t.Fatal("unwire after wire must plan changes")
	}
	if err := Apply(seams, un); err != nil {
		t.Fatal(err)
	}
	sameSnapshot(t, before, snapshot(t, home))

	again, err := PlanUnwire(seams, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if again.Pending() != 0 {
		t.Fatalf("unwire twice must change nothing: %+v", again.Changes)
	}
}

// A registration made by hand before wire ran is not wire's to remove: unwire
// takes back only what the wire plan reported adding, plus the marked block.
func TestUnwireKeepsWhatWireDidNotAdd(t *testing.T) {
	home := t.TempDir()
	mkdirs(t, filepath.Join(home, ".claude"), filepath.Join(home, ".codex"))
	writeFixture(t, filepath.Join(home, ".claude.json"), "{\n  \"mcpServers\": {\n    \"mora\": {\n      \"args\": [\n        \"mcp\",\n        \"serve\"\n      ],\n      \"command\": \"/home/u/.local/bin/mora\"\n    }\n  }\n}\n")
	writeFixture(t, filepath.Join(home, ".codex", "config.toml"), "[mcp_servers.mora]\ncommand = \"/home/u/.local/bin/mora\"\nargs = [\"mcp\", \"serve\"]\n\n[mcp_servers.mora.tools.think]\napproval_mode = \"approve\"\n\n[features]\nflag = true\n")
	seams := wireSeams(t, home, nil)
	before := snapshot(t, home)

	plan, err := PlanWire(seams, WireOptions{Binary: "/opt/mora"})
	if err != nil {
		t.Fatal(err)
	}
	owned := plan.Owned()
	cfg := filepath.Join(home, ".codex", "config.toml")
	if owned[cfg].Registered || len(owned[cfg].Approved) != len(CodexReadOnlyTools)-1 {
		t.Fatalf("wire must own only the approvals it added, got %+v", owned[cfg])
	}
	if _, ok := owned[filepath.Join(home, ".claude.json")]; ok {
		t.Fatalf("an unchanged file must not be owned: %+v", owned)
	}
	if err := Apply(seams, plan); err != nil {
		t.Fatal(err)
	}
	un, err := PlanUnwire(seams, owned, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(seams, un); err != nil {
		t.Fatal(err)
	}
	sameSnapshot(t, before, snapshot(t, home))
}

func TestUnwireLeavesAnApprovalTheUserChangedAfterWire(t *testing.T) {
	home := t.TempDir()
	mkdirs(t, filepath.Join(home, ".codex"))
	seams := wireSeams(t, home, nil)
	plan, err := PlanWire(seams, WireOptions{Binary: "/opt/mora"})
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(seams, plan); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(home, ".codex", "config.toml")
	body := strings.Replace(readFile(t, cfg), "[mcp_servers.mora.tools.brief]\napproval_mode = \"approve\"", "[mcp_servers.mora.tools.brief]\napproval_mode = \"prompt\"", 1)
	writeFixture(t, cfg, body)
	owned := plan.Owned()
	owned[cfg] = Owned{Approved: owned[cfg].Approved} // as if the registration predated wire
	un, err := PlanUnwire(seams, owned, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(seams, un); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, cfg)
	if !strings.Contains(got, "[mcp_servers.mora.tools.brief]\napproval_mode = \"prompt\"") || strings.Contains(got, "tools.search_memory") {
		t.Fatalf("unwire must drop wire's untouched approvals and keep the user's edit:\n%s", got)
	}
	if !strings.Contains(got, "[mcp_servers.mora]") {
		t.Fatalf("a registration wire does not own must stay:\n%s", got)
	}
}

func TestUnwireAllRemovesEveryMoraEntry(t *testing.T) {
	home := t.TempDir()
	mkdirs(t, filepath.Join(home, ".codex"))
	cfg := filepath.Join(home, ".codex", "config.toml")
	writeFixture(t, cfg, "model = \"m\"\n\n[mcp_servers.mora]\ncommand = \"/m\"\nargs = [\"mcp\", \"serve\"]\n")
	seams := wireSeams(t, home, nil)
	un, err := PlanUnwire(seams, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if un.Pending() != 0 {
		t.Fatalf("without ownership or --all, a hand registration stays: %+v", un.Changes)
	}
	un, err = PlanUnwire(seams, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(seams, un); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, cfg); got != "model = \"m\"\n" {
		t.Fatalf("--all must remove the mora entry:\n%q", got)
	}
}

func TestWireDiffNeverShowsOtherConfigKeys(t *testing.T) {
	home := t.TempDir()
	mkdirs(t, filepath.Join(home, ".claude"))
	writeFixture(t, filepath.Join(home, ".claude.json"), `{"oauthAccount":{"emailAddress":"someone@example.com"},"mcpServers":{"other":{"command":"/o","env":{"API_KEY":"sk-synthetic"}}}}`)
	plan, err := PlanWire(wireSeams(t, home, nil), WireOptions{Binary: "/opt/mora"})
	if err != nil {
		t.Fatal(err)
	}
	c := changeFor(t, plan, filepath.Join(home, ".claude.json"))
	for _, leak := range []string{"someone@example.com", "sk-synthetic", "oauthAccount", "other"} {
		if strings.Contains(c.Diff, leak) {
			t.Fatalf("diff leaked %q:\n%s", leak, c.Diff)
		}
	}
	if !strings.Contains(c.Diff, `"command": "/opt/mora"`) {
		t.Fatalf("diff must show the mora entry:\n%s", c.Diff)
	}
}
