package mora

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/resource"
)

// resourceProposal is intentionally separate from memory proposals. A held
// attachment is never fed through the memory parser or later mistaken for a
// vault write approval.
type resourceProposal struct {
	Schema     string             `json:"schema"`
	ID         string             `json:"id"`
	ProposedAt string             `json:"proposed_at"`
	ProposedBy string             `json:"proposed_by,omitempty"`
	Candidate  resource.Candidate `json:"candidate"`
}

func resourceProposalDir(cfg Config) string {
	return filepath.Join(cfg.ConfigDir, "resource-proposals")
}

func resourceCandidateFromArgs(ctx context.Context, args map[string]any) (resource.Candidate, error) {
	p, bound := mcpProjectFrom(ctx)
	if !bound || !mcpWorkspaceManaged(ctx) || p.root == "" || p.scope == "" {
		return resource.Candidate{}, errMCPWorkspaceUnavailable
	}
	kind := resource.Kind(strArg(args, "kind", ""))
	c := resource.Candidate{
		StableID: strArg(args, "stable_id", ""), Kind: kind,
		CanonicalSource: strArg(args, "canonical_source", ""),
		Purpose:         strArg(args, "purpose", ""), EffectiveScope: strArg(args, "scope", p.scope),
		UntrustedScopeSuggestion: strArg(args, "scope_reason", ""),
		Pasted:                   []byte(strArg(args, "pasted", "")), Folder: strArg(args, "folder", ""),
		Reference: resource.Reference{URL: strArg(args, "url", ""), Version: strArg(args, "version", ""), Subdirectory: strArg(args, "subdirectory", "")},
	}
	switch {
	case len(c.Pasted) > 0:
		c.Source = resource.SourcePasted
	case c.Folder != "":
		c.Source = resource.SourceFolder
	case c.Reference.URL != "":
		c.Source = resource.SourceReference
	}
	if c.Folder != "" {
		if !filepath.IsAbs(c.Folder) || filepath.Clean(c.Folder) != c.Folder {
			return resource.Candidate{}, fmt.Errorf("resource attachment must be an absolute canonical project path")
		}
		resolved, err := filepath.EvalSymlinks(c.Folder)
		if err != nil || resolved != c.Folder {
			return resource.Candidate{}, fmt.Errorf("resource attachment is unavailable or aliases a path")
		}
		rel, err := filepath.Rel(p.root, c.Folder)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || !filepath.IsLocal(rel) {
			return resource.Candidate{}, fmt.Errorf("resource attachment is outside the bound project root")
		}
	}
	return c, resource.ValidateCandidate(c)
}

func stageMCPResourceProposalWith(ctx context.Context, cfg Config, args map[string]any, proposedBy string) (any, error) {
	candidate, err := resourceCandidateFromArgs(ctx, args)
	if err != nil {
		return nil, err
	}
	proposal := resourceProposal{Schema: "mora-resource-proposal/v1", ID: "rp_" + newID(), ProposedAt: time.Now().UTC().Format(time.RFC3339), ProposedBy: proposedBy, Candidate: candidate}
	b, err := json.Marshal(proposal)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(resourceProposalDir(cfg), proposal.ID+".json")
	if err := atomicio.WriteDurable(path, append(b, '\n'), 0o600); err != nil {
		return nil, err
	}
	return map[string]any{"proposal": map[string]any{"id": proposal.ID, "status": "pending"}, "message": "resource is held locally for owner review; it was not imported, activated, or fetched"}, nil
}

func mcpSubmitResourceTentative(ctx context.Context, cfg Config, args map[string]any) (any, error) {
	candidate, err := resourceCandidateFromArgs(ctx, args)
	if err != nil {
		return nil, err
	}
	if candidate.Source == resource.SourceFolder || candidate.Kind == resource.KindRepository && candidate.Source != resource.SourceReference {
		return stageMCPResourceProposalWith(ctx, cfg, args, "")
	}
	return importMCPResource(cfg, candidate)
}

func mcpSubmitResource(ctx context.Context, cfg Config, args map[string]any) (any, error) {
	candidate, err := resourceCandidateFromArgs(ctx, args)
	if err != nil {
		return nil, err
	}
	return importMCPResource(cfg, candidate)
}

func importMCPResource(cfg Config, candidate resource.Candidate) (any, error) {
	record, revision, outcome, err := resource.Import(cfg.VaultDir, candidate)
	if err != nil {
		return nil, err
	}
	return map[string]any{"resource": map[string]any{"id": record.StableID, "kind": record.Kind, "revision": revision.RevisionDigest, "tree_digest": revision.TreeDigest, "availability": revision.Availability, "complete": revision.Complete, "review_state": "unreviewed"}, "created": outcome.CreatedRecord || outcome.CreatedRevision, "duplicate": outcome.Duplicate, "activation": "native owner decision required"}, nil
}

func mcpDiscoverResources(ctx context.Context, cfg Config, args map[string]any) (any, error) {
	p, bound := mcpProjectFrom(ctx)
	if !bound || !mcpWorkspaceManaged(ctx) {
		return nil, errMCPWorkspaceUnavailable
	}
	bindings, err := activeResourceBindings(cfg, p)
	if err != nil {
		return nil, err
	}
	rows, err := resource.Discover(cfg.VaultDir, resource.Query{ProjectScope: p.scope, Task: strArg(args, "task", ""), Bindings: bindings})
	if err != nil {
		return nil, err
	}
	return map[string]any{"resources": rows}, nil
}
