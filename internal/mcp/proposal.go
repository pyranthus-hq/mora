package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/config"
)

type Proposal struct {
	ID         string         `json:"id"`
	ProposedAt string         `json:"proposed_at"`
	Arguments  map[string]any `json:"arguments"`
}

func ProposalDir(cfg config.Config) string { return filepath.Join(cfg.ConfigDir, "mcp-proposals") }
func ProposalPath(cfg config.Config, id string) (string, error) {
	if !strings.HasPrefix(id, "p_") || strings.ContainsAny(id, `/\`) || filepath.Base(id) != id {
		return "", fmt.Errorf("invalid MCP proposal id %q", id)
	}
	return filepath.Join(ProposalDir(cfg), id+".json"), nil
}
func SaveProposal(cfg config.Config, proposal Proposal) (string, error) {
	path, err := ProposalPath(cfg, proposal.ID)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	b, err := json.MarshalIndent(proposal, "", "  ")
	if err != nil {
		return "", err
	}
	if err := atomicio.Write(path, append(b, '\n'), 0o600); err != nil {
		return "", err
	}
	return path, nil
}
func ReadProposal(cfg config.Config, id string) (Proposal, string, error) {
	path, err := ProposalPath(cfg, id)
	if err != nil {
		return Proposal{}, "", err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Proposal{}, "", fmt.Errorf("MCP proposal %q not found", id)
		}
		return Proposal{}, "", err
	}
	var proposal Proposal
	if err := json.Unmarshal(b, &proposal); err != nil {
		return Proposal{}, "", fmt.Errorf("parse MCP proposal %q: %w", id, err)
	}
	if proposal.ID != id {
		return Proposal{}, "", fmt.Errorf("MCP proposal id mismatch: file %q contains %q", id, proposal.ID)
	}
	return proposal, path, nil
}
func ListProposals(cfg config.Config) ([]Proposal, error) {
	entries, err := os.ReadDir(ProposalDir(cfg))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	proposals := make([]Proposal, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		proposal, _, err := ReadProposal(cfg, id)
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
