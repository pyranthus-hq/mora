package mora

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/leasefile"
	mcppkg "github.com/pyranthus-hq/mora/internal/mcp"
)

// Decisions are permanent private state, not vault content. In particular, deleting
// a published memory must not make its proposal publishable again.
type mcpProposalDecision struct {
	Version         int    `json:"version"`
	ProposalID      string `json:"proposal_id"`
	Action          string `json:"action"`
	Status          string `json:"status"`
	MemoryID        string `json:"memory_id,omitempty"`
	ArgumentsSHA256 string `json:"arguments_sha256"`
	AttemptedAt     string `json:"attempted_at"`
}

var removeDecidedMCPProposal = os.Remove

func mcpProposalDecisionPath(cfg Config, id string) string {
	return filepath.Join(cfg.StateDir, "mcp-proposal-decisions", fmt.Sprintf("%x.json", sha256.Sum256([]byte(id))))
}

func readMCPProposalDecision(cfg Config, id string) (*mcpProposalDecision, error) {
	path := mcpProposalDecisionPath(cfg, id)
	st, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > 8192 {
		return nil, errors.New("invalid MCP proposal decision record; refusing another decision")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var d mcpProposalDecision
	if json.Unmarshal(body, &d) != nil || d.Version != 1 || d.ProposalID != id ||
		(d.Action != "approve" && d.Action != "reject") ||
		(d.Status != "attempted" && d.Status != "approved" && d.Status != "rejected") ||
		(d.Action == "approve" && d.MemoryID == "") || (d.Action == "reject" && d.MemoryID != "") ||
		(d.Status == "approved" && d.Action != "approve") || (d.Status == "rejected" && d.Action != "reject") || len(d.ArgumentsSHA256) != 64 {
		return nil, errors.New("invalid MCP proposal decision record; refusing another decision")
	}
	if _, err := hex.DecodeString(d.ArgumentsSHA256); err != nil {
		return nil, errors.New("invalid MCP proposal decision digest; refusing another decision")
	}
	if d.MemoryID != "" && (len(d.MemoryID) > 128 || !strings.HasPrefix(d.MemoryID, "mem_") || !proposalInspectionID.MatchString("p_"+strings.TrimPrefix(d.MemoryID, "mem_"))) {
		return nil, errors.New("invalid MCP proposal decision memory ID; refusing another decision")
	}
	if _, err := time.Parse(time.RFC3339Nano, d.AttemptedAt); err != nil {
		return nil, errors.New("invalid MCP proposal decision timestamp; refusing another decision")
	}
	return &d, nil
}

func writeMCPProposalDecision(cfg Config, d *mcpProposalDecision) error {
	body, err := json.Marshal(d)
	if err != nil {
		return err
	}
	return atomicio.WriteDurable(mcpProposalDecisionPath(cfg, d.ProposalID), append(body, '\n'), 0o600)
}

func proposalDecisionConflict(d *mcpProposalDecision) error {
	return fmt.Errorf("conflict: proposal %s already has an %s decision (%s; memory %s); inspect its decision and memory before manual recovery; do not approve again", d.ProposalID, d.Action, d.Status, d.MemoryID)
}

// A durable claim precedes publication. An interrupted attempt fails closed:
// automatic retry cannot distinguish a failed write from a published, deleted,
// or retracted memory. Recovery must reconcile the recorded memory ID manually.
func decideMCPWriteProposal(ctx context.Context, cfg Config, id, action string) (string, error) {
	if action != "approve" && action != "reject" {
		return "", errors.New("invalid proposal decision action")
	}
	if _, err := mcppkg.ProposalPath(cfg, id); err != nil {
		return "", err
	}
	if d, err := readMCPProposalDecision(cfg, id); err != nil {
		return "", err
	} else if d != nil {
		return "", proposalDecisionConflict(d)
	}

	// Refusing malformed legacy writes must not even create lock bookkeeping.
	// Repeat the read/validation under the guard for the authoritative decision.
	p, _, err := readMCPWriteProposal(cfg, id)
	if err != nil {
		// A winner may have removed the pending file since our first decision read.
		if d, decisionErr := readMCPProposalDecision(cfg, id); decisionErr != nil {
			return "", decisionErr
		} else if d != nil {
			return "", proposalDecisionConflict(d)
		}
		return "", err
	}
	if action == "approve" {
		m, err := mcpMemoryFromArgs(p.Arguments, mcpWriteClock())
		if err != nil {
			return "", err
		}
		if err := validateDispositionPublish(cfg, m); err != nil {
			return "", err
		}
	}

	var memoryID string
	err = leasefile.WithGuard(mcpProposalDecisionPath(cfg, id), func() error {
		if d, err := readMCPProposalDecision(cfg, id); err != nil {
			return err
		} else if d != nil {
			return proposalDecisionConflict(d)
		}
		p, path, err := readMCPWriteProposal(cfg, id)
		if err != nil {
			return err
		}
		if action == "approve" {
			m, err := mcpMemoryFromArgs(p.Arguments, mcpWriteClock())
			if err != nil {
				return err
			}
			if err := validateDispositionPublish(cfg, m); err != nil {
				return err
			}
			memoryID = newIDFn()
		}
		arguments, err := json.Marshal(p.Arguments)
		if err != nil {
			return err
		}
		d := &mcpProposalDecision{Version: 1, ProposalID: id, Action: action, Status: "attempted", MemoryID: memoryID, ArgumentsSHA256: fmt.Sprintf("%x", sha256.Sum256(arguments)), AttemptedAt: time.Now().UTC().Format(time.RFC3339Nano)}
		// The guard can create the decisions directory on its first use. Sync
		// its parent before publication so losing that directory cannot erase the
		// claim while keeping the vault write.
		if err := atomicio.SyncDirFn(cfg.StateDir); err != nil {
			return fmt.Errorf("cannot durably establish proposal decision directory; nothing published: %w", err)
		}
		if err := writeMCPProposalDecision(cfg, d); err != nil {
			return fmt.Errorf("cannot durably claim proposal decision; nothing published: %w", err)
		}
		if action == "approve" {
			result, err := mcpWriteMemoryWith(ctx, cfg, p.Arguments, withExplicitMemoryID(memoryID))
			if err != nil {
				return fmt.Errorf("proposal decision remains unresolved for memory %s; do not retry approval: %w", memoryID, err)
			}
			if wrapped, ok := result.(map[string]any); ok && wrapped["already_published"] == true {
				return fmt.Errorf("proposal decision remains unresolved: memory ID %s already exists; refusing collision", memoryID)
			}
			d.Status = "approved"
		} else {
			d.Status = "rejected"
		}
		if err := writeMCPProposalDecision(cfg, d); err != nil {
			return fmt.Errorf("proposal %s decision attempted for memory %s but outcome could not be recorded; do not retry approval: %w", id, memoryID, err)
		}
		if err := removeDecidedMCPProposal(path); err != nil {
			return fmt.Errorf("proposal %s is %s (memory %s), but pending file cleanup failed; do not retry approval: %w", id, d.Status, memoryID, err)
		}
		return nil
	})
	return memoryID, err
}
