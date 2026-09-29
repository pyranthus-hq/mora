package runner

import (
	"github.com/pyranthus-hq/mora/internal/mora/pilotreplay/contract"
)

// Runner is the opt-in local coding replay runner.
type Runner struct {
	ws              *Workspace
	budget          Budget
	isolationLimits []string
	receipts        []AttemptReceipt
}

// Options configures a new Runner.
type Options struct {
	ParentDir string
	Budget    Budget
	// ExtraIsolationLimits are named blockers for real private cases.
	ExtraIsolationLimits []string
}

// New prepares a disposable workspace and returns a Runner.
func New(opts Options) (*Runner, error) {
	ws, err := PrepareWorkspace(opts.ParentDir)
	if err != nil {
		return nil, err
	}
	limits := []string{
		"OS-level network namespace isolation is not verified by this runner; treat unverified network containment as a named blocker for real private cases.",
		"No production vaults, connector stores or external write credentials are used; disposable dirs only.",
		"Paid/external model calls are impossible in ordinary product commands and ordinary test runs.",
		"Real private-case execution stays blocked until #545 (eligibility + oracle + spend authorization).",
		"Runner receipts do not claim historical efficacy.",
	}
	limits = append(limits, opts.ExtraIsolationLimits...)
	return &Runner{
		ws:              ws,
		budget:          opts.Budget,
		isolationLimits: limits,
		receipts:        []AttemptReceipt{},
	}, nil
}

// Workspace returns the disposable layout.
func (r *Runner) Workspace() *Workspace { return r.ws }

// Budget returns a copy of the session budget.
func (r *Runner) Budget() Budget { return r.budget }

// Receipts returns all attempt receipts recorded this session.
func (r *Runner) Receipts() []AttemptReceipt {
	out := make([]AttemptReceipt, len(r.receipts))
	copy(out, r.receipts)
	return out
}

// IsolationLimits returns named blockers / assumptions.
func (r *Runner) IsolationLimits() []string {
	out := make([]string, len(r.isolationLimits))
	copy(out, r.isolationLimits)
	return out
}

// DeniedGate returns a gate that must reject provider invocation.
func DeniedGate() contract.RunGate {
	return contract.ExampleDeniedRunGate()
}

// SyntheticPlumbingGate is a shape-valid gate for synthetic local plumbing.
// It does not authorize real paid runs. Monetary ceiling is set for shape
// completeness; synthetic contenders never spend.
func SyntheticPlumbingGate() contract.RunGate {
	g := contract.ExampleAuthorizedRunGate()
	g.GateID = "gate-synth-plumbing-local-001"
	g.Notes = "Synthetic local plumbing gate. Does not authorize real paid/external provider runs."
	return g
}
