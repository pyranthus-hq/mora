package mora

import (
	"context"
	"errors"
	"fmt"
	configstore "github.com/pyranthus-hq/mora/internal/config"
	mcppkg "github.com/pyranthus-hq/mora/internal/mcp"
	"io"
	"time"
)

const (
	mcpWritePolicyOpen     = mcppkg.WritePolicyOpen
	mcpWritePolicyPropose  = mcppkg.WritePolicyPropose
	mcpWritePolicyReadonly = mcppkg.WritePolicyReadonly
)

func parseMCPWritePolicy(raw string) (string, error) { return configstore.ParseMCPWritePolicy(raw) }

// unsetMCPWritePolicy is what an install without an mcp_write_policy line
// resolves to. In production it is always mcppkg.DefaultWritePolicy (propose).
// It is a variable only so this package's TestMain can keep the many older
// tests that exercise open-mode writes after a bare `mora init` on their old
// behavior; write_policy_default_test.go restores the shipped value and pins it.
var unsetMCPWritePolicy = mcppkg.DefaultWritePolicy

// configMCPWritePolicy is the single resolver every MCP and companion write
// path uses. An explicit value always wins; an unset value fails safe.
func configMCPWritePolicy(c Config) string {
	if c.MCPWritePolicy == "" {
		return unsetMCPWritePolicy
	}
	return mcppkg.NormalizeWritePolicy(c.MCPWritePolicy)
}

type mcpWriteProposal = mcppkg.Proposal

func mcpProposalDir(cfg Config) string { return mcppkg.ProposalDir(cfg) }
func readMCPWriteProposal(cfg Config, id string) (mcpWriteProposal, string, error) {
	return mcppkg.ReadProposal(cfg, id)
}
func listMCPWriteProposals(cfg Config) ([]mcpWriteProposal, error) { return mcppkg.ListProposals(cfg) }

func stageMCPWriteProposal(cfg Config, args map[string]any) (any, error) {
	return stageMCPWriteProposalBy(cfg, args, "")
}

// stageMCPWriteProposalBy stages a propose-mode write and records which agent
// profile proposed it ("" for an unprofiled client), so the owner reviewing
// the queue sees who is asking.
func stageMCPWriteProposalBy(cfg Config, args map[string]any, proposedBy string) (any, error) {
	now := mcpWriteClock()
	m, err := mcpMemoryFromArgs(args, now)
	if err != nil {
		return nil, err
	}
	if err := validateDispositionPublish(cfg, m); err != nil {
		return nil, err
	}
	proposal := mcpWriteProposal{ID: "p_" + newID(), ProposedAt: now.Format(time.RFC3339), ProposedBy: proposedBy, Arguments: args}
	if _, err := mcppkg.SaveProposal(cfg, proposal); err != nil {
		return nil, err
	}
	return map[string]any{
		"proposal": map[string]any{"id": proposal.ID, "status": "pending", "proposed_at": proposal.ProposedAt},
		"message":  fmt.Sprintf("not written to the vault; inspect with `mora mcp proposals show %s` before approving with `mora mcp proposals approve %s`", proposal.ID, proposal.ID),
		"health":   compactHealthOf(cfg, now),
	}, nil
}

// mcpProposalRow is one pending propose-mode write, projected for machines.
type mcpProposalRow struct {
	ID         string `json:"id"`
	ProposedAt string `json:"proposed_at"`
	ProposedBy string `json:"proposed_by,omitempty"`
	Scope      string `json:"scope"`
	Type       string `json:"type"`
	Title      string `json:"title"`
	Status     string `json:"status,omitempty"`
	MemoryID   string `json:"memory_id,omitempty"`
}

// mcpProposalsPayload carries the queue under a named key, never null.
type mcpProposalsPayload struct {
	Proposals  []mcpProposalRow       `json:"proposals"`
	Decision   string                 `json:"decision,omitempty"`
	MemoryID   string                 `json:"memory_id,omitempty"`
	Inspection *mcpProposalInspection `json:"inspection,omitempty"`
}

func cmdMCPProposals(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	cfg, err := loadConfigFor(ctx)
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
				fmt.Fprintf(stderr, "warning: skipping invalid proposal %s: %v\n", proposal.ID, err)
				continue
			}
			row := mcpProposalRow{
				ID: proposal.ID, ProposedAt: proposal.ProposedAt, ProposedBy: proposal.ProposedBy,
				Scope: memory.Scope, Type: memory.Type, Title: memory.Title,
			}
			d, err := readMCPProposalDecision(cfg, proposal.ID)
			if err != nil {
				return err
			}
			if d != nil {
				row.Status, row.MemoryID = d.Status, d.MemoryID
			}
			rows = append(rows, row)
		}
		if jsonOut {
			return emitReceipt(stdout, "mora.mcp.proposals", 1, mcpProposalsPayload{Proposals: rows})
		}
		if len(rows) == 0 {
			fmt.Fprintln(stdout, "No pending MCP write proposals.")
			return nil
		}
		for _, row := range rows {
			by := ""
			if row.ProposedBy != "" {
				by = "  by " + row.ProposedBy
			}
			fmt.Fprintf(stdout, "%s  %s%s  [%s/%s] %s", row.ID, row.ProposedAt, by, row.Scope, row.Type, row.Title)
			if row.Status != "" {
				fmt.Fprintf(stdout, "  [decision: %s; memory: %s; do not approve again]", row.Status, row.MemoryID)
			}
			fmt.Fprintln(stdout)
		}
		return nil
	}
	if len(args) == 2 && args[0] == "show" {
		if err := refuseDashLedPositional("mcp proposals show", "proposal id", args[1]); err != nil {
			return err
		}
		return showMCPWriteProposal(cfg, args[1], stdout)
	}
	if len(args) == 2 && args[0] == "approve" {
		if err := refuseDashLedPositional("mcp proposals approve", "proposal id", args[1]); err != nil {
			return err
		}
		memoryID, err := decideMCPWriteProposal(ctx, cfg, args[1], "approve")
		if err != nil {
			return err
		}

		if jsonOut {
			return emitReceipt(stdout, "mora.mcp.proposals", 1, mcpProposalsPayload{
				Proposals: []mcpProposalRow{{ID: args[1]}}, Decision: "approve", MemoryID: memoryID,
			})
		}
		fmt.Fprintf(stdout, "Approved %s as memory %s.\n", args[1], memoryID)
		return nil
	}
	if len(args) == 2 && args[0] == "reject" {
		if err := refuseDashLedPositional("mcp proposals reject", "proposal id", args[1]); err != nil {
			return err
		}
		if _, err := decideMCPWriteProposal(ctx, cfg, args[1], "reject"); err != nil {
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
	return errors.New("usage: mora mcp proposals <list|show ID|approve ID|reject ID>")
}
