package mora

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/integrations"
)

const (
	schemaAgentsWire   = "mora.agents.wire"
	schemaAgentsUnwire = "mora.agents.unwire"
	agentsUsage        = "usage: mora agents wire [--dry-run] [--binary <abs>] [--json] | unwire [--dry-run] [--all] [--json]"
	agentsOwnedFile    = "agents-wire.json"
)

// agentsOwnedRecord lives in the state dir, never the vault. It names what
// wire added to each agent file, so unwire can take back exactly that and
// leave a registration someone made by hand.
type agentsOwnedRecord struct {
	Schema string                        `json:"schema"`
	Files  map[string]integrations.Owned `json:"files"`
}

func agentsOwnedPath(cfg Config) string { return filepath.Join(cfg.StateDir, agentsOwnedFile) }

func loadAgentsOwned(cfg Config) (map[string]integrations.Owned, error) {
	body, err := os.ReadFile(agentsOwnedPath(cfg))
	if os.IsNotExist(err) {
		return map[string]integrations.Owned{}, nil
	}
	if err != nil {
		return nil, err
	}
	var rec agentsOwnedRecord
	if err := json.Unmarshal(body, &rec); err != nil {
		return nil, fmt.Errorf("%s: %w", agentsOwnedPath(cfg), err)
	}
	if rec.Files == nil {
		rec.Files = map[string]integrations.Owned{}
	}
	return rec.Files, nil
}

func saveAgentsOwned(cfg Config, files map[string]integrations.Owned) error {
	path := agentsOwnedPath(cfg)
	if len(files) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	body, err := json.MarshalIndent(agentsOwnedRecord{Schema: "mora.agents.owned", Files: files}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return err
	}
	return atomicio.Write(path, append(body, '\n'), 0o600)
}

type agentsPayload struct {
	DryRun  bool                      `json:"dry_run"`
	Pending int                       `json:"pending"`
	Changes []integrations.FileChange `json:"changes"`
}

// cmdAgents wires every detected agent (Claude Code, each Codex home including
// Orca-managed ones, Pi) to Mora, or unwires it. It always prints the diff of
// the Mora-owned text before writing, and --dry-run stops there.
func cmdAgents(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) < 1 || (args[0] != "wire" && args[0] != "unwire") {
		return newCodedError(errCodeUsageUnknownValue, nil, "%s", agentsUsage)
	}
	verb := args[0]
	fs := flag.NewFlagSet("agents "+verb, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dryRun := fs.Bool("dry-run", false, "show what would change; write nothing")
	jsonOut := fs.Bool("json", false, "emit JSON")
	var binary *string
	var all *bool
	if verb == "wire" {
		binary = fs.String("binary", "", "absolute path a new MCP registration runs (default: this executable)")
	} else {
		all = fs.Bool("all", false, "also remove Mora registrations wire did not add")
	}
	if err := fs.Parse(args[1:]); err != nil {
		return newMoraError(errCodeUsageUnknownFlag, "usage", err, "%v", err)
	}
	if fs.NArg() != 0 {
		return newCodedError(errCodeUsageUnknownValue, nil, "%s", agentsUsage)
	}
	cfg, err := loadConfigFor(ctx)
	if err != nil {
		return err
	}
	seams := integrationSeams(cfg)
	owned, err := loadAgentsOwned(cfg)
	if err != nil {
		return newCodedError(errCodeInternalUnexpected, err, "agents %s: read wire record: %v", verb, err)
	}

	var plan integrations.Plan
	schema := schemaAgentsUnwire
	if verb == "wire" {
		schema = schemaAgentsWire
		bin := *binary
		if bin == "" {
			if bin, err = currentExecutable(); err != nil {
				return newCodedError(errCodeInternalUnexpected, err, "resolve executable: %v", err)
			}
		} else if !filepath.IsAbs(bin) {
			return newCodedError(errCodeUsageUnknownValue, nil, "--binary must be an absolute path, got %q", bin)
		}
		plan, err = integrations.PlanWire(seams, integrations.WireOptions{Binary: bin})
	} else {
		plan, err = integrations.PlanUnwire(seams, owned, *all)
	}
	if err != nil {
		return newCodedError(errCodeInternalUnexpected, err, "agents %s: %v", verb, err)
	}

	if !*jsonOut {
		printAgentsPlan(stdout, verb, plan, *dryRun)
	}
	if !*dryRun && plan.Pending() > 0 {
		// Record ownership before writing: a crash mid-apply must still let
		// unwire find what was added.
		if verb == "wire" {
			if err := saveAgentsOwned(cfg, integrations.MergeOwned(owned, plan.Owned())); err != nil {
				return newCodedError(errCodeInternalUnexpected, err, "agents wire: save wire record: %v", err)
			}
		}
		if err := integrations.Apply(seams, plan); err != nil {
			return newCodedError(errCodeInternalUnexpected, err, "agents %s: %v", verb, err)
		}
	}
	if !*dryRun && verb == "unwire" {
		if err := saveAgentsOwned(cfg, nil); err != nil {
			return newCodedError(errCodeInternalUnexpected, err, "agents unwire: clear wire record: %v", err)
		}
	}
	if *jsonOut {
		changes := plan.Changes
		if changes == nil {
			changes = []integrations.FileChange{}
		}
		return emitReceipt(stdout, schema, 1, agentsPayload{DryRun: *dryRun, Pending: plan.Pending(), Changes: changes})
	}
	return nil
}

func currentExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe, nil
}

func printAgentsPlan(w io.Writer, verb string, plan integrations.Plan, dryRun bool) {
	if len(plan.Changes) == 0 {
		if verb == "wire" {
			fmt.Fprintln(w, "no agents detected (looked for ~/.claude, ~/.codex, $CODEX_HOME, Orca-managed Codex homes, ~/.pi/agent)")
		} else {
			fmt.Fprintln(w, "nothing to unwire: no Mora entry or block found")
		}
		return
	}
	for _, c := range plan.Changes {
		line := fmt.Sprintf("%-7s %-13s %s  %s", c.Agent, c.Kind, c.Path, c.Action)
		if c.Note != "" {
			line += " (" + c.Note + ")"
		}
		fmt.Fprintln(w, line)
		if c.Diff != "" {
			fmt.Fprint(w, c.Diff)
		}
	}
	n := plan.Pending()
	switch {
	case n == 0:
		fmt.Fprintf(w, "nothing to change: already %sd\n", verb)
	case dryRun:
		fmt.Fprintf(w, "dry run: %d file(s) would change; nothing written\n", n)
	default:
		fmt.Fprintf(w, "%sd: %d file(s) changed\n", verb, n)
	}
}
