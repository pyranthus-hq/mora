package mora

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
			result, err := mcpWriteMemoryWith(withAgentCaller(ctx, "approved-proposal"), cfg, p.Arguments, withExplicitMemoryID(memoryID))
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

// lookupPublishedMemory reports whether memory id is in the vault. Absence is
// proved, not assumed: a failed directory walk, an unreadable file, or a corrupt
// file that could be this memory is an error, never "not found". want, when
// known, gives the exact path createMemory would have used.
func lookupPublishedMemory(cfg Config, id string, want *Memory) (Memory, bool, error) {
	if want != nil {
		w := *want
		w.ID = id
		path, err := memoryPath(cfg, w)
		if err != nil {
			return Memory{}, false, err
		}
		if _, err := os.Lstat(path); err == nil {
			m, perr := parseMemory(path)
			if perr != nil {
				return Memory{}, false, fmt.Errorf("memory file %s exists but cannot be read: %w", path, perr)
			}
			return m, true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return Memory{}, false, err
		}
	}
	files, err := allMemoryFiles(cfg)
	if err != nil {
		return Memory{}, false, err
	}
	for _, path := range files {
		m, perr := parseMemory(path)
		if perr != nil {
			if strings.Contains(filepath.Base(path), id) {
				return Memory{}, false, fmt.Errorf("memory file %s cannot be read: %w", path, perr)
			}
			continue
		}
		if m.ID == id {
			return m, true, nil
		}
	}
	return Memory{}, false, nil
}

// sameProposalMemory refuses unless every proposal-controlled field the vault
// file persists matches the proposal. Time-derived defaults are computed at the
// file's own created_at so a later retry does not look like a difference.
func sameProposalMemory(found Memory, args map[string]any) error {
	created, err := time.Parse(time.RFC3339, found.CreatedAt)
	if err != nil {
		return fmt.Errorf("vault memory has an invalid created_at: %w", err)
	}
	want, err := mcpMemoryFromArgs(args, created)
	if err != nil {
		return err
	}
	same := found.Scope == want.Scope && found.Type == want.Type && found.Title == want.Title &&
		strings.TrimSpace(found.Text) == strings.TrimSpace(want.Text) && found.Source == want.Source
	if same {
		a, _ := json.Marshal([]any{found.Meta, found.Decision})
		b, _ := json.Marshal([]any{want.Meta, want.Decision})
		same = bytes.Equal(a, b)
	}
	if !same {
		return errors.New("differs")
	}
	return nil
}

// recoverMCPWriteProposal finishes a decision that a crash left half done. It runs
// under the same guard as approve and reject, prints what it found and what it
// did, and never publishes a memory twice. The one case it cannot decide alone is
// an attempted approval with no vault file: the audit log cannot prove the memory
// never published, so the owner picks --retry or --close. It returns the
// proposal's state afterwards ("approved", "rejected" or "pending") and the
// memory id, if any.
func recoverMCPWriteProposal(cfg Config, id string, retry, closeIt bool, stdout io.Writer) (status, memoryID string, err error) {
	path, err := mcppkg.ProposalPath(cfg, id)
	if err != nil {
		return "", "", err
	}
	if retry && closeIt {
		return "", "", errors.New("choose --retry or --close, not both")
	}
	err = leasefile.WithGuard(mcpProposalDecisionPath(cfg, id), func() error {
		d, err := readMCPProposalDecision(cfg, id)
		if err != nil {
			return err
		}
		if d == nil {
			return fmt.Errorf("nothing to recover: proposal %s has no decision record", id)
		}
		_, statErr := os.Lstat(path)
		hasFile := statErr == nil
		finish := func(st string) error {
			if hasFile {
				if err := removeDecidedMCPProposal(path); err != nil {
					return fmt.Errorf("proposal %s is %s, but pending file cleanup failed: %w", id, st, err)
				}
			}
			status, memoryID = st, d.MemoryID
			fmt.Fprintf(stdout, "Recovered %s: decision is %s; pending file removed.\n", id, st)
			return nil
		}
		if d.Status != "attempted" || d.Action == "reject" {
			if retry || closeIt {
				return errors.New("--retry and --close only apply to an attempted approval with no vault file")
			}
			if d.Status == "attempted" {
				fmt.Fprintln(stdout, "Found: attempted reject, nothing published.")
				d.Status = "rejected"
				if err := writeMCPProposalDecision(cfg, d); err != nil {
					return err
				}
				return finish("rejected")
			}
			fmt.Fprintf(stdout, "Found: %s decision is %s.\n", d.Action, d.Status)
			return finish(d.Status)
		}
		// Attempted approve: establish whether the memory is really in the vault.
		var p mcpWriteProposal
		var want *Memory
		if hasFile {
			if p, _, err = readMCPWriteProposal(cfg, id); err != nil {
				return err
			}
			m, err := mcpMemoryFromArgs(p.Arguments, mcpWriteClock())
			if err != nil {
				return err
			}
			want = &m
		}
		found, present, err := lookupPublishedMemory(cfg, d.MemoryID, want)
		if err != nil {
			return fmt.Errorf("cannot tell whether memory %s is in the vault; refusing to recover: %w", d.MemoryID, err)
		}
		if present {
			if retry || closeIt {
				return errors.New("--retry and --close only apply to an attempted approval with no vault file")
			}
			fmt.Fprintf(stdout, "Found: attempted approve; memory %s is in the vault.\n", d.MemoryID)
			if !hasFile {
				return fmt.Errorf("cannot check memory %s against the proposal: the pending file is gone; inspect the memory by hand", d.MemoryID)
			}
			args, err := json.Marshal(p.Arguments)
			if err != nil {
				return err
			}
			if fmt.Sprintf("%x", sha256.Sum256(args)) != d.ArgumentsSHA256 {
				return fmt.Errorf("collision: proposal %s changed after the decision was recorded; refusing to mark memory %s approved", id, d.MemoryID)
			}
			if err := sameProposalMemory(found, p.Arguments); err != nil {
				return fmt.Errorf("collision: memory %s in the vault differs from proposal %s (%v); refusing to mark it approved", d.MemoryID, id, err)
			}
			d.Status = "approved"
			if err := writeMCPProposalDecision(cfg, d); err != nil {
				return err
			}
			scheduleAuthoredReconciliation(cfg)
			return finish("approved")
		}
		hint := "the audit log has no write line for it"
		if logged, lerr := agentWriteLogged(cfg, d.MemoryID); lerr == nil && logged {
			hint = "the audit log shows it was published and later removed"
		}
		fmt.Fprintf(stdout, "Found: attempted approve; memory %s is not in the vault (%s). A missing log line proves nothing.\n", d.MemoryID, hint)
		switch {
		case retry:
			if err := os.Remove(mcpProposalDecisionPath(cfg, id)); err != nil {
				return err
			}
			status = "pending"
			fmt.Fprintf(stdout, "Recovered %s: decision file removed; the proposal is pending again.\n", id)
			return nil
		case closeIt:
			d.Action, d.Status, d.MemoryID = "reject", "rejected", ""
			if err := writeMCPProposalDecision(cfg, d); err != nil {
				return err
			}
			return finish("rejected")
		}
		return errors.New("choose --retry (approve again later) or --close (keep the memory absent)")
	})
	return status, memoryID, err
}
