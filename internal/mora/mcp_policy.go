package mora

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pyranthus-hq/mora/internal/atomicio"
	configstore "github.com/pyranthus-hq/mora/internal/config"
	mcppkg "github.com/pyranthus-hq/mora/internal/mcp"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	mcpWritePolicyOpen     = mcppkg.WritePolicyOpen
	mcpWritePolicyPropose  = mcppkg.WritePolicyPropose
	mcpWritePolicyReadonly = mcppkg.WritePolicyReadonly
)

func parseMCPWritePolicy(raw string) (string, error) { return configstore.ParseMCPWritePolicy(raw) }
func configMCPWritePolicy(c Config) string           { return mcppkg.NormalizeWritePolicy(c.MCPWritePolicy) }

func mcpInstructionsFor(policy string) string { return mcppkg.InstructionsFor(policy) }

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
	if err := atomicio.Write(path, append(b, '\n'), 0o600); err != nil {
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

func cmdMCPProposals(ctx context.Context, args []string, stdout io.Writer) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if len(args) == 1 && args[0] == "list" {
		proposals, err := listMCPWriteProposals(cfg)
		if err != nil {
			return err
		}
		if len(proposals) == 0 {
			fmt.Fprintln(stdout, "No pending MCP write proposals.")
			return nil
		}
		for _, proposal := range proposals {
			memory, err := mcpMemoryFromArgs(proposal.Arguments, time.Now())
			if err != nil {
				return fmt.Errorf("proposal %s is invalid: %w", proposal.ID, err)
			}
			fmt.Fprintf(stdout, "%s  %s  [%s/%s] %s\n", proposal.ID, proposal.ProposedAt, memory.Scope, memory.Type, memory.Title)
		}
		return nil
	}
	if len(args) == 2 && args[0] == "approve" {
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
		fmt.Fprintf(stdout, "Approved %s as memory %s.\n", proposal.ID, memoryID)
		return nil
	}
	if len(args) == 2 && args[0] == "reject" {
		_, path, err := readMCPWriteProposal(cfg, args[1])
		if err != nil {
			return err
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Rejected %s.\n", args[1])
		return nil
	}
	return errors.New("usage: mora mcp proposals <list|approve ID|reject ID>")
}
