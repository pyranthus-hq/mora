package mora

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/pyranthus-hq/mora/internal/integrations"
	mcppkg "github.com/pyranthus-hq/mora/internal/mcp"
)

// withAgentsHome isolates `mora agents` from the developer's machine: a temp
// HOME, and no CODEX_HOME or Orca data dir that would point outside it.
func withAgentsHome(t *testing.T) string {
	t.Helper()
	withTempHomeSetenv(t)
	t.Setenv("CODEX_HOME", "")
	t.Setenv("ORCA_USER_DATA_PATH", "")
	cfg, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	home := cfg.HomeDir()
	for _, d := range []string{".claude", ".codex", filepath.Join(".config", "orca", "codex-accounts", "a", "home"), filepath.Join(".pi", "agent")} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func treeOf(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() && !strings.Contains(p, string(filepath.Separator)+"vault"+string(filepath.Separator)) {
			b, _ := os.ReadFile(p)
			out[p] = string(b)
		}
		return nil
	})
	return out
}

func agentFiles(t *testing.T, home string) map[string]string {
	t.Helper()
	all := treeOf(t, home)
	out := map[string]string{}
	for p, b := range all {
		for _, f := range []string{".claude.json", "CLAUDE.md", "AGENTS.md", "config.toml"} {
			if filepath.Base(p) == f {
				out[p] = b
			}
		}
	}
	return out
}

func TestAgentsWireDryRunShowsDiffAndWritesNothing(t *testing.T) {
	home := withAgentsHome(t)
	before := agentFiles(t, home)
	out, _, err := runSplit(t, "agents", "wire", "--dry-run", "--binary", "/opt/mora/bin/mora")
	if err != nil {
		t.Fatal(err)
	}
	if after := agentFiles(t, home); len(after) != len(before) {
		t.Fatalf("dry run wrote files: %v", after)
	}
	for _, want := range []string{
		filepath.Join(home, ".claude", "CLAUDE.md"),
		filepath.Join(home, ".codex", "config.toml"),
		filepath.Join(home, ".config", "orca", "codex-accounts", "a", "home", "AGENTS.md"),
		filepath.Join(home, ".pi", "agent", "AGENTS.md"),
		"+" + integrations.BlockBegin,
		`+approval_mode = "approve"`,
		"dry run",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, out)
		}
	}
}

func TestAgentsWireTwiceThenUnwire(t *testing.T) {
	home := withAgentsHome(t)
	before := agentFiles(t, home)
	if _, _, err := runSplit(t, "agents", "wire", "--binary", "/opt/mora/bin/mora"); err != nil {
		t.Fatal(err)
	}
	wired := agentFiles(t, home)
	if len(wired) != 7 {
		t.Fatalf("want 7 wired files, got %d: %v", len(wired), agentPaths(wired))
	}
	out, _, err := runSplit(t, "agents", "wire", "--binary", "/opt/mora/bin/mora", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var receipt struct {
		Schema  string `json:"schema"`
		DryRun  bool   `json:"dry_run"`
		Pending int    `json:"pending"`
		Changes []struct {
			Path   string `json:"path"`
			Action string `json:"action"`
		} `json:"changes"`
	}
	if err := json.Unmarshal([]byte(out), &receipt); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if receipt.Schema != "mora.agents.wire" || receipt.Pending != 0 || len(receipt.Changes) != 7 {
		t.Fatalf("second wire must be a no-op: %+v", receipt)
	}
	if again := agentFiles(t, home); !sameMap(wired, again) {
		t.Fatal("second wire changed files")
	}
	if _, _, err := runSplit(t, "agents", "unwire"); err != nil {
		t.Fatal(err)
	}
	if after := agentFiles(t, home); !sameMap(before, after) {
		t.Fatalf("unwire did not restore the agent files: %v", agentPaths(after))
	}
}

// Registrations that existed before wire (devbox's hand-made ones) survive
// unwire; --all removes them too.
func TestAgentsUnwireKeepsAHandRegistrationUnlessAll(t *testing.T) {
	home := withAgentsHome(t)
	cfgPath := filepath.Join(home, ".codex", "config.toml")
	hand := "[mcp_servers.mora]\ncommand = \"/home/u/.local/bin/mora\"\nargs = [\"mcp\", \"serve\"]\n"
	if err := os.WriteFile(cfgPath, []byte(hand), 0o600); err != nil {
		t.Fatal(err)
	}
	before := agentFiles(t, home)
	if _, _, err := runSplit(t, "agents", "wire", "--binary", "/opt/mora/bin/mora"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runSplit(t, "agents", "unwire"); err != nil {
		t.Fatal(err)
	}
	if after := agentFiles(t, home); !sameMap(before, after) {
		t.Fatalf("unwire must restore the pre-wire files, keeping the hand registration: %v", agentPaths(after))
	}
	if _, _, err := runSplit(t, "agents", "unwire", "--all"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(cfgPath); strings.Contains(string(b), "mcp_servers.mora") {
		t.Fatalf("unwire --all must remove the hand registration:\n%s", b)
	}
}

// The contract harness runs every registered command, `agents wire` included,
// under a temp home. TestMain clears CODEX_HOME and ORCA_USER_DATA_PATH so an
// inherited Orca session can never point a test at a real Codex home.
func TestAgentsTestsStartWithoutAgentHomeEnv(t *testing.T) {
	for _, k := range []string{"CODEX_HOME", "ORCA_USER_DATA_PATH"} {
		if v := os.Getenv(k); v != "" {
			t.Fatalf("%s=%q leaked into the test process", k, v)
		}
	}
}

func TestAgentsUsageErrors(t *testing.T) {
	withAgentsHome(t)
	for _, args := range [][]string{{"agents"}, {"agents", "bogus"}, {"agents", "wire", "--binary", "relative/mora"}, {"agents", "wire", "extra"}, {"agents", "wire", "--all"}} {
		if _, _, err := runSplit(t, args...); err == nil {
			t.Errorf("%v: want a usage error", args)
		}
	}
}

// Every MCP tool must be classified for Codex: pre-approved because it only
// reads, or left to Codex's approval policy because it writes. A new tool
// fails here until someone decides which.
func TestCodexApprovalCoversEveryMCPTool(t *testing.T) {
	mutating := map[string]bool{"write_memory": true, "delete_memory": true}
	readOnly := map[string]bool{}
	for _, name := range integrations.CodexReadOnlyTools {
		readOnly[name] = true
	}
	var unclassified []string
	for _, def := range mcppkg.ToolCatalog() {
		if readOnly[def.Name] == mutating[def.Name] {
			unclassified = append(unclassified, def.Name)
		}
	}
	if len(unclassified) != 0 || len(readOnly)+len(mutating) != len(mcppkg.ToolCatalog()) {
		t.Fatalf("classify these MCP tools as read-only or mutating: %v", unclassified)
	}
}

func agentPaths(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sameMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}
