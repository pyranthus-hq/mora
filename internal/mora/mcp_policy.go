package mora

import (
	"context"
	"errors"
	"fmt"
	configstore "github.com/pyranthus-hq/mora/internal/config"
	mcppkg "github.com/pyranthus-hq/mora/internal/mcp"
	"io"
	"os"
	"time"
)

const (
	mcpWritePolicyOpen     = mcppkg.WritePolicyOpen
	mcpWritePolicyPropose  = mcppkg.WritePolicyPropose
	mcpWritePolicyReadonly = mcppkg.WritePolicyReadonly
)

func parseMCPWritePolicy(raw string) (string, error) { return configstore.ParseMCPWritePolicy(raw) }
func configMCPWritePolicy(c Config) string           { return mcppkg.NormalizeWritePolicy(c.MCPWritePolicy) }

type mcpWriteProposal = mcppkg.Proposal

func mcpProposalDir(cfg Config) string { return mcppkg.ProposalDir(cfg) }
func readMCPWriteProposal(cfg Config, id string) (mcpWriteProposal, string, error) {
	return mcppkg.ReadProposal(cfg, id)
}
func listMCPWriteProposals(cfg Config) ([]mcpWriteProposal, error) { return mcppkg.ListProposals(cfg) }

func stageMCPWriteProposal(cfg Config, args map[string]any) (any, error) {
	now := mcpWriteClock()
	if _, err := mcpMemoryFromArgs(args, now); err != nil {
		return nil, err
	}
	proposal := mcpWriteProposal{ID: "p_" + newID(), ProposedAt: now.Format(time.RFC3339), Arguments: args}
	if _, err := mcppkg.SaveProposal(cfg, proposal); err != nil {
		return nil, err
	}
	return map[string]any{
		"proposal": map[string]any{"id": proposal.ID, "status": "pending", "proposed_at": proposal.ProposedAt},
		"message":  fmt.Sprintf("not written to the vault; the owner can inspect and approve with `mora mcp proposals approve %s`", proposal.ID),
		"health":   compactHealthOf(cfg, now),
	}, nil
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
