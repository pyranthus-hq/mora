package mora

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	mcpWritePolicyOpen     = "open"
	mcpWritePolicyPropose  = "propose"
	mcpWritePolicyReadonly = "readonly"
)

func parseMCPWritePolicy(raw string) (string, error) {
	policy := strings.ToLower(strings.TrimSpace(raw))
	switch policy {
	case mcpWritePolicyOpen, mcpWritePolicyPropose, mcpWritePolicyReadonly:
		return policy, nil
	default:
		return "", fmt.Errorf("invalid mcp_write_policy %q (want open, propose, or readonly)", raw)
	}
}

func (c Config) mcpWritePolicy() string {
	if c.MCPWritePolicy == "" {
		return mcpWritePolicyOpen
	}
	return c.MCPWritePolicy
}

const openWriteInstruction = "Write durable facts and decisions back with write_memory as they emerge — you do not need to ask permission."

func mcpInstructionsFor(policy string) string {
	var replacement string
	switch policy {
	case mcpWritePolicyPropose:
		replacement = "You may submit durable facts and decisions with write_memory, but they enter a pending proposal queue and are NOT part of the vault until the owner approves them locally. delete_memory is unavailable in this mode."
	case mcpWritePolicyReadonly:
		replacement = "This connection is read-only: do not call write_memory or delete_memory; both will refuse without changing the vault."
	default:
		replacement = openWriteInstruction
	}
	return strings.Replace(mcpInstructions, openWriteInstruction, replacement, 1)
}

type mcpWriteProposal struct {
	ID         string         `json:"id"`
	ProposedAt string         `json:"proposed_at"`
	Arguments  map[string]any `json:"arguments"`
}

func mcpProposalDir(cfg Config) string {
	return filepath.Join(cfg.ConfigDir, "mcp-proposals")
}

func mcpProposalPath(cfg Config, id string) (string, error) {
	if !strings.HasPrefix(id, "p_") || strings.ContainsAny(id, `/\\`) || filepath.Base(id) != id {
		return "", fmt.Errorf("invalid MCP proposal id %q", id)
	}
	return filepath.Join(mcpProposalDir(cfg), id+".json"), nil
}

func stageMCPWriteProposal(cfg Config, args map[string]any) (any, error) {
	now := mcpWriteClock()
	if _, err := mcpMemoryFromArgs(args, now); err != nil {
		return nil, err
	}
	proposal := mcpWriteProposal{ID: "p_" + newID(), ProposedAt: now.Format(time.RFC3339), Arguments: args}
	path, err := mcpProposalPath(cfg, proposal.ID)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	b, err := json.MarshalIndent(proposal, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := atomicWrite(path, append(b, '\n'), 0o600); err != nil {
		return nil, err
	}
	return map[string]any{
		"proposal": map[string]any{"id": proposal.ID, "status": "pending", "proposed_at": proposal.ProposedAt},
		"message":  fmt.Sprintf("not written to the vault; the owner can inspect and approve with `mora mcp proposals approve %s`", proposal.ID),
		"health":   compactHealthOf(cfg, now),
	}, nil
}

func readMCPWriteProposal(cfg Config, id string) (mcpWriteProposal, string, error) {
	path, err := mcpProposalPath(cfg, id)
	if err != nil {
		return mcpWriteProposal{}, "", err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return mcpWriteProposal{}, "", fmt.Errorf("MCP proposal %q not found", id)
		}
		return mcpWriteProposal{}, "", err
	}
	var proposal mcpWriteProposal
	if err := json.Unmarshal(b, &proposal); err != nil {
		return mcpWriteProposal{}, "", fmt.Errorf("parse MCP proposal %q: %w", id, err)
	}
	if proposal.ID != id {
		return mcpWriteProposal{}, "", fmt.Errorf("MCP proposal id mismatch: file %q contains %q", id, proposal.ID)
	}
	return proposal, path, nil
}

func listMCPWriteProposals(cfg Config) ([]mcpWriteProposal, error) {
	entries, err := os.ReadDir(mcpProposalDir(cfg))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	proposals := make([]mcpWriteProposal, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		proposal, _, err := readMCPWriteProposal(cfg, id)
		if err != nil {
			return nil, err
		}
		proposals = append(proposals, proposal)
	}
	sort.Slice(proposals, func(i, j int) bool {
		if proposals[i].ProposedAt == proposals[j].ProposedAt {
			return proposals[i].ID < proposals[j].ID
		}
		return proposals[i].ProposedAt < proposals[j].ProposedAt
	})
	return proposals, nil
}

// mcpProposalRow is one pending propose-mode write, projected for machines.
type mcpProposalRow struct {
	ID         string `json:"id"`
	ProposedAt string `json:"proposed_at"`
	Scope      string `json:"scope"`
	Type       string `json:"type"`
	Title      string `json:"title"`
}

// mcpProposalsPayload carries the queue under a named key, never null.
type mcpProposalsPayload struct {
	Proposals []mcpProposalRow `json:"proposals"`
	Decision  string           `json:"decision,omitempty"`
	MemoryID  string           `json:"memory_id,omitempty"`
}

func cmdMCPProposals(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	jsonOut := false
	rest := make([]string, 0, len(args))
	for _, a := range args {
		if a == "--json" {
			jsonOut = true
			continue
		}
		rest = append(rest, a)
	}
	args = rest
	if len(args) == 1 && args[0] == "list" {
		proposals, err := listMCPWriteProposals(cfg)
		if err != nil {
			return err
		}
		rows := make([]mcpProposalRow, 0, len(proposals))
		for _, proposal := range proposals {
			memory, err := mcpMemoryFromArgs(proposal.Arguments, time.Now())
			if err != nil {
				return fmt.Errorf("proposal %s is invalid: %w", proposal.ID, err)
			}
			rows = append(rows, mcpProposalRow{
				ID: proposal.ID, ProposedAt: proposal.ProposedAt,
				Scope: memory.Scope, Type: memory.Type, Title: memory.Title,
			})
		}
		if jsonOut {
			return emitReceipt(stdout, "mora.mcp.proposals", 1, mcpProposalsPayload{Proposals: rows})
		}
		if len(rows) == 0 {
			fmt.Fprintln(stdout, "No pending MCP write proposals.")
			return nil
		}
		for _, row := range rows {
			fmt.Fprintf(stdout, "%s  %s  [%s/%s] %s\n", row.ID, row.ProposedAt, row.Scope, row.Type, row.Title)
		}
		return nil
	}
	if len(args) == 2 && args[0] == "approve" {
		if err := refuseDashLedPositional("mcp proposals approve", "proposal id", args[1]); err != nil {
			return err
		}
		proposal, path, err := readMCPWriteProposal(cfg, args[1])
		if err != nil {
			return err
		}
		result, err := mcpWriteMemory(ctx, cfg, proposal.Arguments)
		if err != nil {
			return err
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("proposal %s was written to the vault but its pending file could not be removed: %w", proposal.ID, err)
		}
		memoryID := ""
		if wrapped, ok := result.(map[string]any); ok {
			if memory, ok := wrapped["memory"].(Memory); ok {
				memoryID = memory.ID
			}
		}
		if jsonOut {
			return emitReceipt(stdout, "mora.mcp.proposals", 1, mcpProposalsPayload{
				Proposals: []mcpProposalRow{{ID: proposal.ID}}, Decision: "approve", MemoryID: memoryID,
			})
		}
		fmt.Fprintf(stdout, "Approved %s as memory %s.\n", proposal.ID, memoryID)
		return nil
	}
	if len(args) == 2 && args[0] == "reject" {
		if err := refuseDashLedPositional("mcp proposals reject", "proposal id", args[1]); err != nil {
			return err
		}
		_, path, err := readMCPWriteProposal(cfg, args[1])
		if err != nil {
			return err
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		if jsonOut {
			return emitReceipt(stdout, "mora.mcp.proposals", 1, mcpProposalsPayload{
				Proposals: []mcpProposalRow{{ID: args[1]}}, Decision: "reject",
			})
		}
		fmt.Fprintf(stdout, "Rejected %s.\n", args[1])
		return nil
	}
	return errors.New("usage: mora mcp proposals <list|approve ID|reject ID>")
}
