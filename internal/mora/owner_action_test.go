package mora

import (
	"bytes"
	"errors"
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

func TestPublicOwnerActionsFailClosedWithoutVerifier(t *testing.T) {
	withUnavailableOwnerVerifier(t)
	cfg := reviewSetup(t)
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
			if policy.wantErr {
				if err == nil {
					t.Fatal("readonly capture wrote")
				}
				return
			}
			if err != nil {
				t.Fatalf("write = %v; %s", err, errOut.String())
			}
			files, err := allMemoryFiles(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if (len(files) == 1) != policy.wantMemory {
				t.Fatalf("memory files = %v, wantMemory=%v", files, policy.wantMemory)
			}
			if policy.wantMemory {
				m, err := parseMemory(files[0])
				if err != nil || memory.IsTentative(m) != policy.wantTent || memory.IsOwnerConfirmed(m) {
					t.Fatalf("captured memory = %+v, %v", m, err)
				}
			}
			if !policy.wantMemory {
				if pending, err := pendingMCPProposalCount(cfg); err != nil || pending != 1 {
					t.Fatalf("pending capture proposal = %d, %v", pending, err)
				}
			}
			before, _ := os.ReadFile(filepath.Join(cfg.VaultDir, "live-tasks.md"))
			if err := Run(testCtx(t), []string{"tasks", "add", "policy task"}, ioDiscard{}, ioDiscard{}, nil); err == nil && policy.write != mcpWritePolicyOpen {
				t.Fatal("non-open task mutation succeeded")
			}
			after, _ := os.ReadFile(filepath.Join(cfg.VaultDir, "live-tasks.md"))
			if policy.write != mcpWritePolicyOpen && !bytes.Equal(before, after) {
				t.Fatal("refused task mutation changed live-tasks.md")
			}
		})
	}
}
