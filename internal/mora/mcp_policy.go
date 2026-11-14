package mora

import (
	"context"
	"errors"
	"fmt"
	configstore "github.com/pyranthus-hq/mora/internal/config"
	mcppkg "github.com/pyranthus-hq/mora/internal/mcp"
	"io"
	"strings"
	"time"
)

const (
	mcpWritePolicyOpen     = mcppkg.WritePolicyOpen
	mcpWritePolicyPropose  = mcppkg.WritePolicyPropose
	mcpWritePolicyReadonly = mcppkg.WritePolicyReadonly
)

// unsetMCPWritePolicy is what an install without an mcp_write_policy line
// resolves to. In production it is always mcppkg.DefaultWritePolicy. It is a
// variable only so tests can pin the shipped default while exercising an
// isolated configuration. Production always uses mcppkg.DefaultWritePolicy.
var unsetMCPWritePolicy = mcppkg.DefaultWritePolicy

func parseMCPWritePolicy(raw string) (string, error) { return configstore.ParseMCPWritePolicy(raw) }

// configMCPWritePolicy is the single resolver every MCP and companion write
// path uses. An explicit value always wins; an unset value resolves to
// mcppkg.DefaultWritePolicy (tentative).
func configMCPWritePolicy(c Config) string {
	if c.MCPWritePolicy == "" {
		return unsetMCPWritePolicy
	}
	return mcppkg.NormalizeWritePolicy(c.MCPWritePolicy)
}

// cliCaptureAction applies the configured mutation policy to raw CLI capture.
// It is intentionally independent of environment markers: those describe a
// caller, not authority or the configured storage disposition.
func cliCaptureAction(cfg Config) (string, error) {
	return mcppkg.MutationAction(configMCPWritePolicy(cfg), "write_memory")
}

func cliTaskMutationAllowed(cfg Config) error {
	action, err := cliCaptureAction(cfg)
	if err != nil {
		return err
	}
	if action != mcppkg.ActionExecute {
		return fmt.Errorf("task mutation refused: mcp_write_policy=%s; tasks have no exact pending-operation queue yet", configMCPWritePolicy(cfg))
	}
	return nil
}

// stageCLICapture reuses the normal proposal queue, preserving the authored
// memory fields instead of creating a second raw-CLI staging format.
func stageCLICapture(cfg Config, m Memory, heldBecause []string) (any, error) {
	if err := refuseOwnerConfirmedTags(m.Tags); err != nil {
		return nil, err
	}
	if len(m.Tags) != 0 {
		return nil, errors.New("CLI proposal staging does not support tags; remove --tags before staging")
	}
	args := map[string]any{
		"scope": m.Scope, "type": m.Type, "title": m.Title, "text": m.Text,
		"source": m.Source,
	}
	if m.Meta != nil {
		if target, ok := m.Meta["target"].(string); ok {
			args["target"] = target
		}
		if disposition, ok := m.Meta["disposition"].(string); ok {
			args["disposition"] = disposition
		}
	}
	if m.Decision != nil {
		args["as_of"] = m.Decision.AsOf
		args["durability"] = m.Decision.Durability
		args["flip_conditions"] = strings.Join(m.Decision.FlipConditions, ";")
		args["review_by"] = m.Decision.ReviewBy
	}
	return stageMCPWriteProposalHeld(cfg, args, "cli", heldBecause)
}

type mcpWriteProposal = mcppkg.Proposal

func mcpProposalDir(cfg Config) string { return mcppkg.ProposalDir(cfg) }
func readMCPWriteProposal(cfg Config, id string) (mcpWriteProposal, string, error) {
	return mcppkg.ReadProposal(cfg, id)
}
func listMCPWriteProposals(cfg Config) ([]mcpWriteProposal, error) { return mcppkg.ListProposals(cfg) }

func stageMCPWriteProposal(cfg Config, args map[string]any) (any, error) {
	return stageMCPWriteProposalHeld(cfg, args, "", nil)
}

// mcpWriteTentative is write_memory under the tentative policy. A write that
// passes tentativeHoldReasons saves at once; any other goes to the same queue
// propose uses, and the result names why.
func mcpWriteTentative(ctx context.Context, cfg Config, args map[string]any) (any, error) {
	m, err := mcpMemoryFromArgs(args, mcpWriteClock())
	if err != nil {
		return nil, err
	}
	if reasons := tentativeHoldReasons(m); len(reasons) > 0 {
		return stageMCPWriteProposalHeld(cfg, args, "", reasons)
	}
	return mcpWriteMemoryWith(ctx, cfg, args, withTentative())
}

// stageMCPWriteProposalHeld stages a write. proposedBy names the agent profile
// ("" for an unprofiled client), so the owner reviewing the queue sees who is
// asking; heldBecause says why a tentative write did not save at once.
func stageMCPWriteProposalHeld(cfg Config, args map[string]any, proposedBy string, heldBecause []string) (any, error) {
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
	result := map[string]any{
		"proposal": map[string]any{"id": proposal.ID, "status": "pending", "proposed_at": proposal.ProposedAt},
		"message":  fmt.Sprintf("not written to the vault; inspect with `mora mcp proposals show %s` before approving with `mora mcp proposals approve %s`", proposal.ID, proposal.ID),
		"health":   compactHealthOf(cfg, now),
	}
	if len(heldBecause) > 0 {
		result["held_because"] = heldBecause
	}
	if n := reviewNotice(context.Background(), cfg); n != "" {
		result["review_notice"] = n
	}
	return result, nil
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

// approveDrainLimit bounds how long approve waits for the rebuild it started.
var approveDrainLimit = 2 * time.Minute

func drainReconcileOrWarn(stderr io.Writer) {
	if !waitForAuthoredReconciles(approveDrainLimit) {
		fmt.Fprintln(stderr, "warning: the memory is saved, but the index rebuild is unfinished; run `mora index rebuild`")
	}
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
	if len(args) >= 2 && args[0] == "approve" {
		ids := args[1:]
		for _, id := range ids {
			if err := requireVerifiedOwnerAction(ownerAction{Kind: "proposal.approve", TargetID: id}); err != nil {
				return err
			}
			if err := refuseDashLedPositional("mcp proposals approve", "proposal id", id); err != nil {
				return err
			}
		}
		// approve starts background rebuilds and then exits, which would kill them and
		// leave a dead receipt. Drain once after the last id, also on failure returns.
		defer drainReconcileOrWarn(stderr)
		rows := make([]mcpProposalRow, 0, len(ids))
		var failure error
		for _, id := range ids {
			// A retried batch treats an id that already finished as done.
			if d, derr := readMCPProposalDecision(cfg, id); derr == nil && d != nil && d.Action == "approve" && d.Status == "approved" {
				rows = append(rows, mcpProposalRow{ID: id, Status: "approved", MemoryID: d.MemoryID})
				if !jsonOut {
					fmt.Fprintf(stdout, "Already approved %s as memory %s.\n", id, d.MemoryID)
				}
				continue
			}
			memoryID, err := decideMCPWriteProposal(ctx, cfg, id, "approve")
			if err != nil {
				rows = append(rows, mcpProposalRow{ID: id, Status: "failed"})
				failure = err
				break
			}
			rows = append(rows, mcpProposalRow{ID: id, Status: "approved", MemoryID: memoryID})
			if !jsonOut {
				fmt.Fprintf(stdout, "Approved %s as memory %s.\n", id, memoryID)
			}
		}
		if jsonOut {
			payload := mcpProposalsPayload{Proposals: rows, Decision: "approve"}
			if len(rows) == 1 && failure == nil {
				payload.MemoryID = rows[0].MemoryID
			}
			if err := emitReceipt(stdout, "mora.mcp.proposals", 1, payload); err != nil {
				return err
			}
		}
		return failure
	}
	if len(args) >= 2 && args[0] == "recover" {
		retry, closeIt := false, false
		for _, a := range args[2:] {
			switch a {
			case "--retry":
				retry = true
			case "--close":
				closeIt = true
			default:
				return fmt.Errorf("usage: mora mcp proposals recover ID [--retry|--close]; unknown argument %q", a)
			}
		}
		if err := refuseDashLedPositional("mcp proposals recover", "proposal id", args[1]); err != nil {
			return err
		}
		defer drainReconcileOrWarn(stderr)
		text := stdout
		if jsonOut {
			text = io.Discard
		}
		status, memoryID, err := recoverMCPWriteProposal(cfg, args[1], retry, closeIt, text)
		if err != nil || !jsonOut {
			return err
		}
		return emitReceipt(stdout, "mora.mcp.proposals", 1, mcpProposalsPayload{
			Proposals: []mcpProposalRow{{ID: args[1], Status: status, MemoryID: memoryID}}, Decision: "recover", MemoryID: memoryID,
		})
	}
	if len(args) == 2 && args[0] == "reject" {
		if err := refuseDashLedPositional("mcp proposals reject", "proposal id", args[1]); err != nil {
			return err
		}
		if err := requireVerifiedOwnerAction(ownerAction{Kind: "proposal.reject", TargetID: args[1]}); err != nil {
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
	return errors.New("usage: mora mcp proposals <list|show ID|approve ID [ID...]|reject ID|recover ID [--retry|--close]>")
}
