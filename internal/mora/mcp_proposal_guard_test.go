package mora

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mcppkg "github.com/pyranthus-hq/mora/internal/mcp"
)

// stageGuardProposal stages one write and returns its id, as an agent's
// write_memory call does under the propose policy.
func stageGuardProposal(t *testing.T, cfg Config) string {
	t.Helper()
	p := mcpWriteProposal{ID: "p_guard", ProposedAt: "2026-11-05T00:00:00Z", Arguments: map[string]any{"title": "Use pnpm", "text": "Always install pnpm"}}
	if _, err := mcppkg.SaveProposal(cfg, p); err != nil {
		t.Fatal(err)
	}
	return p.ID
}

// assertProposalUndecided fails unless the proposal is still pending, has no
// decision record, and reached no memory file.
func assertProposalUndecided(t *testing.T, cfg Config, id string) {
	t.Helper()
	if proposals, err := listMCPWriteProposals(cfg); err != nil || len(proposals) != 1 || proposals[0].ID != id {
		t.Fatalf("pending proposals = %#v, err = %v", proposals, err)
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "mcp-proposal-decisions")); !os.IsNotExist(err) {
		t.Fatalf("a refused decision must write nothing, stat err = %v", err)
	}
	if files, err := allMemoryFiles(cfg); err != nil || len(files) != 0 {
		t.Fatalf("memory files = %v, err = %v", files, err)
	}
}

// proposalDecisionCommands are the two commands the guard covers, with and
// without --json. The flag must not open a way around the check.
func proposalDecisionCommands(id string) [][]string {
	return [][]string{
		{"proposals", "approve", id},
		{"proposals", "reject", id},
		{"proposals", "approve", id, "--json"},
		{"proposals", "reject", id, "--json"},
	}
}

func TestMCPProposalDecisionRefusesNonInteractive(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	clearAgentEnv(t)
	cfg := mustConfig(t)
	id := stageGuardProposal(t, cfg)
	prev := rulesInteractive
	rulesInteractive = func(io.Reader) bool { return false }
	t.Cleanup(func() { rulesInteractive = prev })
	for _, args := range proposalDecisionCommands(id) {
		var out bytes.Buffer
		err := cmdMCP(testCtx(t), args, &out, &out, strings.NewReader(""))
		if err == nil || err.Error() != mcpProposalRefusal {
			t.Fatalf("%v: err = %v", args, err)
		}
		if out.Len() != 0 {
			t.Fatalf("%v: stdout = %q", args, out.String())
		}
		assertProposalUndecided(t, cfg, id)
	}
	// The real gate refuses the non-TTY stdin the test harness passes.
	rulesInteractive = prev
	for _, args := range proposalDecisionCommands(id) {
		var out bytes.Buffer
		if err := cmdMCP(testCtx(t), args, &out, &out, strings.NewReader("")); err == nil || err.Error() != mcpProposalRefusal {
			t.Fatalf("non-TTY stdin: %v: err = %v", args, err)
		}
		assertProposalUndecided(t, cfg, id)
	}
}

func TestMCPProposalDecisionRefusesInsideAgentSession(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	id := stageGuardProposal(t, cfg)
	for _, key := range rulesAgentEnv {
		subRun(t, key, func(t *testing.T) {
			stubRulesTerminal(t, "")
			t.Setenv(key, "1")
			want := fmt.Sprintf(mcpProposalAgentRefusal, key)
			for _, args := range proposalDecisionCommands(id) {
				var out bytes.Buffer
				if err := cmdMCP(testCtx(t), args, &out, &out, strings.NewReader("")); err == nil || err.Error() != want {
					t.Fatalf("%v: err = %v", args, err)
				}
				assertProposalUndecided(t, cfg, id)
			}
		})
	}
}

// Each command must run for a person at a terminal, or the guard is a lockout.
func TestMCPProposalDecisionRunsAtOwnTerminal(t *testing.T) {
	for _, tc := range []struct {
		action, want string
		memories     int
	}{
		{"approve", "Approved p_guard as memory", 1},
		{"reject", "Rejected p_guard.", 0},
	} {
		subRun(t, tc.action, func(t *testing.T) {
			withTempHome(t)
			run(t, "init")
			cfg := mustConfig(t)
			id := stageGuardProposal(t, cfg)
			stubRulesTerminal(t, "")
			var out bytes.Buffer
			if err := cmdMCP(testCtx(t), []string{"proposals", tc.action, id}, &out, &out, strings.NewReader("")); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), tc.want) {
				t.Fatalf("stdout = %q, want %q", out.String(), tc.want)
			}
			if files, err := allMemoryFiles(cfg); err != nil || len(files) != tc.memories {
				t.Fatalf("memory files = %v, err = %v, want %d", files, err, tc.memories)
			}
			if proposals, err := listMCPWriteProposals(cfg); err != nil || len(proposals) != 0 {
				t.Fatalf("pending proposals = %#v, err = %v", proposals, err)
			}
		})
	}
}

// Codex user settings such as CODEX_HOME live in people's own shell profiles.
// They must not lock the person out of deciding a proposal.
func TestMCPProposalDecisionAllowsCodexUserSettings(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	id := stageGuardProposal(t, cfg)
	stubRulesTerminal(t, "")
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), ".codex"))
	t.Setenv("CODEX_API_KEY", "x")
	var out bytes.Buffer
	if err := cmdMCP(testCtx(t), []string{"proposals", "approve", id}, &out, &out, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
}

// An agent still needs to read the queue, so list and show stay open in an
// agent session on a non-terminal stdin.
func TestMCPProposalReadsRunInsideAgentSession(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	id := stageGuardProposal(t, cfg)
	t.Setenv("CLAUDECODE", "1")
	for _, args := range [][]string{{"proposals", "list"}, {"proposals", "show", id}} {
		var out bytes.Buffer
		if err := cmdMCP(testCtx(t), args, &out, &out, strings.NewReader("")); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if !strings.Contains(out.String(), id) {
			t.Fatalf("%v: stdout = %q", args, out.String())
		}
	}
	assertProposalUndecided(t, cfg, id)
}
