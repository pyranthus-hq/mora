package mora

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mcppkg "github.com/pyranthus-hq/mora/internal/mcp"
	"github.com/pyranthus-hq/mora/internal/memory"
)

func withUnavailableOwnerVerifier(t *testing.T) {
	t.Helper()
	previous := ownerActionVerifier
	ownerActionVerifier = func(ownerAction) error { return errOwnerVerificationUnavailable }
	t.Cleanup(func() { ownerActionVerifier = previous })
}

// legacyOpenPolicy is an explicit fixture choice for tests that exercise the
// historical direct-write kernel. It is never installed by TestMain or shared
// home setup, so bare-init/default-policy tests still execute production
// tentative behavior.
func legacyOpenPolicy(t *testing.T) {
	t.Helper()
	cfg := mustConfig(t)
	cfg.MCPWritePolicy = mcpWritePolicyOpen
	if err := writeConfig(cfg); err != nil {
		t.Fatal(err)
	}
}

// withTestVerifiedOwnerAction is an explicit, per-test verifier for legacy
// kernel/provenance fixtures. It is never installed by TestMain, and public
// route tests keep the production-unavailable verifier unless they opt in.
func withTestVerifiedOwnerAction(t *testing.T) {
	t.Helper()
	previous := ownerActionVerifier
	ownerActionVerifier = func(action ownerAction) error {
		if action.Kind == "" {
			return errors.New("test owner verifier requires an action kind")
		}
		if key := rulesAgentSession(); key != "" {
			if strings.HasPrefix(action.Kind, "rule.") {
				return fmt.Errorf(rulesAgentRefusal, key)
			}
			return fmt.Errorf("refusing %s from inside an agent session (%s is set). The owner can run it from their own terminal.", action.Kind, key)
		}
		return nil
	}
	t.Cleanup(func() { ownerActionVerifier = previous })
}

func TestPublicOwnerActionsFailClosedWithoutVerifier(t *testing.T) {
	cfg := reviewSetup(t)
	withUnavailableOwnerVerifier(t)
	clearAgentEnv(t) // A cleared marker and a fake terminal are still not proof.
	previousInteractive := rulesInteractive
	rulesInteractive = func(io.Reader) bool { return true }
	t.Cleanup(func() { rulesInteractive = previousInteractive })

	if _, _, err := mineRun(t, strings.NewReader("yes\n"), append(mineArgs("write", "--mine"), "--json")...); !errors.Is(err, errOwnerVerificationUnavailable) {
		t.Fatalf("marker-cleared write --mine = %v", err)
	}
	note := reviewSaved(t, "Public confirm denial")
	if err := Run(testCtx(t), []string{"confirm", note}, ioDiscard{}, ioDiscard{}, strings.NewReader("yes\n")); !errors.Is(err, errOwnerVerificationUnavailable) {
		t.Fatalf("marker-cleared confirm = %v", err)
	}
	if !reviewTentative(t, cfg, note) {
		t.Fatal("denied confirm changed the memory")
	}
	held := reviewHeld(t, "Public proposal denial")
	if err := Run(testCtx(t), []string{"mcp", "proposals", "approve", held}, ioDiscard{}, ioDiscard{}, nil); !errors.Is(err, errOwnerVerificationUnavailable) {
		t.Fatalf("marker-cleared proposal approval = %v", err)
	}
	if d, err := readMCPProposalDecision(cfg, held); err != nil || d != nil {
		t.Fatalf("denied proposal approval changed state: %v, %v", d, err)
	}
	run(t, "rules", "add", "--template", "compound-cd")
	if err := Run(testCtx(t), []string{"rules", "approve", "compound-cd", "--mode", "warn"}, ioDiscard{}, ioDiscard{}, strings.NewReader("compound-cd\n")); !errors.Is(err, errOwnerVerificationUnavailable) {
		t.Fatalf("marker-cleared rule approval = %v", err)
	}
	if err := cmdForget(testCtx(t), []string{"--chat", "owner-proof-probe", "--yes"}, ioDiscard{}, ioDiscard{}); !errors.Is(err, errOwnerVerificationUnavailable) {
		t.Fatalf("marker-cleared forget = %v", err)
	}
	if err := cmdUnforget(testCtx(t), []string{"owner-proof-probe", "--yes"}, ioDiscard{}, ioDiscard{}); !errors.Is(err, errOwnerVerificationUnavailable) {
		t.Fatalf("marker-cleared unforget = %v", err)
	}
	for name, args := range map[string][]string{
		"handoff mine":    {"handoff", "write", "--mine"},
		"teach memory":    {"teach", "memory", "correct"},
		"teach consent":   {"teach", "consent", "enable"},
		"merge":           {"merge", "confirm"},
		"rule off":        {"rules", "off", "compound-cd"},
		"rule grant":      {"rules", "grant", "compound-cd", "--to", "project:demo", "--as", "owner", "--key", "/missing.pub"},
		"proposal reject": {"mcp", "proposals", "reject", "missing"},
	} {
		if err := Run(testCtx(t), args, ioDiscard{}, ioDiscard{}, strings.NewReader("yes\n")); !errors.Is(err, errOwnerVerificationUnavailable) {
			t.Fatalf("marker-cleared %s = %v", name, err)
		}
	}
}

func TestCLIDestructivePolicyRefusesBeforeMutationAndOpenExecutes(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	cfg.MCPWritePolicy = mcppkg.WritePolicyTentative
	if err := writeConfig(cfg); err != nil {
		t.Fatal(err)
	}
	seed := func(title string) Memory {
		t.Helper()
		m, _, err := createMemory(testCtx(t), cfg, Memory{Scope: "project:demo", Type: "fact", Title: title, Text: "body", Source: "manual"})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	for _, operation := range []struct {
		name string
		run  func(Memory) error
	}{
		{"delete", func(m Memory) error { return cmdDelete(testCtx(t), []string{"--yes", m.ID}, ioDiscard{}, ioDiscard{}) }},
		{"undo-force", func(m Memory) error { return cmdUndo(testCtx(t), []string{"--force", m.ID}, ioDiscard{}, ioDiscard{}) }},
	} {
		t.Run(operation.name, func(t *testing.T) {
			for _, policy := range []string{mcppkg.WritePolicyTentative, mcpWritePolicyPropose, mcpWritePolicyReadonly} {
				m := seed("policy " + operation.name + " " + policy)
				cfg.MCPWritePolicy = policy
				if err := writeConfig(cfg); err != nil {
					t.Fatal(err)
				}
				if err := operation.run(m); err == nil {
					t.Fatalf("%s %s = %v", policy, operation.name, err)
				}
				if _, err := findMemory(cfg, m.ID); err != nil {
					t.Fatalf("refused %s changed memory: %v", operation.name, err)
				}
			}
			m := seed("open " + operation.name)
			cfg.MCPWritePolicy = mcpWritePolicyOpen
			if err := writeConfig(cfg); err != nil {
				t.Fatal(err)
			}
			if err := operation.run(m); err != nil {
				t.Fatalf("open %s = %v", operation.name, err)
			}
			if _, err := findMemory(cfg, m.ID); err == nil {
				t.Fatalf("open %s did not delete memory", operation.name)
			}
			cfg.MCPWritePolicy = mcppkg.WritePolicyTentative
			if err := writeConfig(cfg); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCLICaptureAndTaskPolicyMatrix(t *testing.T) {
	for _, policy := range []struct {
		name       string
		write      string
		wantMemory bool
		wantTent   bool
		wantErr    bool
	}{
		{name: "open", write: mcpWritePolicyOpen, wantMemory: true},
		{name: "tentative", write: mcppkg.WritePolicyTentative, wantMemory: true, wantTent: true},
		{name: "propose", write: mcpWritePolicyPropose},
		{name: "readonly", write: mcpWritePolicyReadonly, wantErr: true},
	} {
		t.Run(policy.name, func(t *testing.T) {
			withTempHome(t)
			run(t, "init")
			cfg := mustConfig(t)
			cfg.MCPWritePolicy = policy.write
			if err := writeConfig(cfg); err != nil {
				t.Fatal(err)
			}
			var out, errOut bytes.Buffer
			err := Run(testCtx(t), []string{"write", "--scope", "project:demo", "--type", "fact", "--title", "CLI policy", "--text", "body", "--json"}, &out, &errOut, nil)
			if policy.wantErr && err == nil {
				t.Fatal("readonly capture wrote")
			}
			if !policy.wantErr && err != nil {
				t.Fatalf("write = %v; %s", err, errOut.String())
			}
			if policy.wantErr {
				if err := Run(testCtx(t), []string{"tasks", "add", "policy task"}, ioDiscard{}, ioDiscard{}, nil); err == nil {
					t.Fatal("readonly task mutation succeeded")
				}
			}
			handoffOut, handoffRunErr := hoCmd(t, cfg, "write", "--scope", "project:demo", "--title", "CLI policy handoff", "--text", "Direction: retain policy")
			if policy.wantErr {
				if handoffRunErr == nil {
					t.Fatalf("readonly handoff mutation succeeded: %q", handoffOut)
				}
			} else if handoffRunErr != nil {
				t.Fatalf("handoff capture = %v", handoffRunErr)
			}
			files, err := allMemoryFiles(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if policy.wantMemory {
				if len(files) != 2 {
					t.Fatalf("captured memory files = %v, want raw write plus handoff", files)
				}
				seen := map[string]Memory{}
				for _, path := range files {
					m, err := parseMemory(path)
					if err != nil {
						t.Fatal(err)
					}
					seen[m.Title] = m
				}
				for title, wantType := range map[string]string{"CLI policy": "fact", "CLI policy handoff": memory.TypeHandoff} {
					m, ok := seen[title]
					if !ok || m.Type != wantType || memory.IsTentative(m) != policy.wantTent || memory.IsOwnerConfirmed(m) {
						t.Fatalf("captured %q = %+v, found=%v", title, m, ok)
					}
				}
			} else if policy.write == mcpWritePolicyPropose {
				if pending, err := pendingMCPProposalCount(cfg); err != nil || pending != 2 {
					t.Fatalf("pending capture proposals = %d, %v; want 2", pending, err)
				}
			} else if len(files) != 0 {
				t.Fatalf("readonly capture left memory files: %v", files)
			} else if pending, err := pendingMCPProposalCount(cfg); err != nil || pending != 0 {
				t.Fatalf("readonly capture proposals = %d, %v", pending, err)
			}
			before, _ := os.ReadFile(filepath.Join(cfg.VaultDir, "live-tasks.md"))
			taskAddErr := Run(testCtx(t), []string{"tasks", "add", "policy task"}, ioDiscard{}, ioDiscard{}, nil)
			if policy.write == mcpWritePolicyOpen && taskAddErr != nil {
				t.Fatalf("open task add = %v", taskAddErr)
			}
			if policy.write != mcpWritePolicyOpen && taskAddErr == nil {
				t.Fatal("non-open task mutation succeeded")
			}
			doneErr := Run(testCtx(t), []string{"tasks", "done", "policy task"}, ioDiscard{}, ioDiscard{}, nil)
			syncErr := Run(testCtx(t), []string{"tasks", "sync", "--write"}, ioDiscard{}, ioDiscard{}, nil)
			if policy.write == mcpWritePolicyOpen && (doneErr != nil || syncErr != nil) {
				t.Fatalf("open task done/sync = %v / %v", doneErr, syncErr)
			}
			if policy.write != mcpWritePolicyOpen && (doneErr == nil || syncErr == nil) {
				t.Fatalf("non-open task done/sync = %v / %v", doneErr, syncErr)
			}
			after, _ := os.ReadFile(filepath.Join(cfg.VaultDir, "live-tasks.md"))
			if policy.write != mcpWritePolicyOpen && !bytes.Equal(before, after) {
				t.Fatal("refused task mutation changed live-tasks.md")
			}
		})
	}
}

func TestCLIProposalStagingPreservesDecisionFieldsOrRefusesTags(t *testing.T) {
	withTestVerifiedOwnerAction(t)
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	cfg.MCPWritePolicy = mcpWritePolicyPropose
	if err := writeConfig(cfg); err != nil {
		t.Fatal(err)
	}
	m := Memory{
		Scope: "project:demo", Type: "decision", Title: "Staged decision", Text: "Keep the verified field values.", Source: "manual",
		Decision: &DecisionValidity{AsOf: "2026-11-13T00:00:00Z", Durability: decisionStanding, FlipConditions: []string{"first condition", "second condition"}, ReviewBy: "2026-12-01T00:00:00Z"},
	}
	staged, err := stageCLICapture(cfg, m, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending := staged.(map[string]any)["proposal"].(map[string]any)
	id := pending["id"].(string)
	memoryID, err := decideMCPWriteProposal(testCtx(t), cfg, id, "approve")
	if err != nil {
		t.Fatal(err)
	}
	stored, err := findMemory(cfg, memoryID)
	if err != nil || stored.Decision == nil || strings.Join(stored.Decision.FlipConditions, "|") != "first condition|second condition" || stored.Decision.ReviewBy != m.Decision.ReviewBy {
		t.Fatalf("staged decision = %+v, %v", stored, err)
	}
	m.Tags = []string{"keep"}
	if _, err := stageCLICapture(cfg, m, nil); err == nil || !strings.Contains(err.Error(), "does not support tags") {
		t.Fatalf("tagged proposal = %v", err)
	}
	m.Tags = []string{memory.OwnerConfirmedTagPrefix + "forged"}
	if _, err := stageCLICapture(cfg, m, nil); err == nil || !strings.Contains(err.Error(), "reserved prefix") {
		t.Fatalf("reserved tag proposal = %v", err)
	}
}
