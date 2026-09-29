package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/pyranthus-hq/mora/internal/mora/pilotreplay/contract"
)

// UsageRecord captures observed usage / cost / unknowns for one attempt.
type UsageRecord struct {
	TokensObserved int      `json:"tokens_observed"`
	CostUSDMicros  int64    `json:"cost_usd_micros"`
	CostKnown      bool     `json:"cost_known"`
	Unknowns       []string `json:"unknowns"`
	AccountingNote string   `json:"accounting_note,omitempty"`
}

// IsolationReport records containment observations for one attempt.
type IsolationReport struct {
	ResetObserved          bool     `json:"reset_observed"`
	InitialHashesMatched   bool     `json:"initial_hashes_matched"`
	ContenderWrites        []string `json:"contender_writes"`
	OracleLeakDetected     bool     `json:"oracle_leak_detected"`
	OracleLeakPaths        []string `json:"oracle_leak_paths,omitempty"`
	ProductionPathRejected bool     `json:"production_path_rejected"`
	NamedLimitations       []string `json:"named_limitations"`
}

// AttemptReceipt is the full runner receipt for one attempt. It is not a
// historical efficacy claim.
type AttemptReceipt struct {
	Attempt         contract.AttemptDocument `json:"attempt"`
	Exposure        ExposureRecord           `json:"exposure"`
	Digests         InputDigests             `json:"digests"`
	OutputRefs      []string                 `json:"output_refs"`
	ExitClass       string                   `json:"exit_class"`
	Usage           UsageRecord              `json:"usage"`
	Isolation       IsolationReport          `json:"isolation"`
	ProviderStarted bool                     `json:"provider_started"`
	ClaimsEfficacy  bool                     `json:"claims_efficacy"` // must remain false
	RunnerNotes     string                   `json:"runner_notes,omitempty"`
}

// TrialRequest is one opt-in trial invocation.
type TrialRequest struct {
	Case           contract.CaseDocument
	Condition      contract.ConditionDocument
	Gate           contract.RunGate
	RepIndex       int
	TimeoutSeconds int
	Contender      Contender
	// SkipReset is for internal negative tests only; ordinary trials reset.
	SkipReset bool
}

// RunTrial executes one trial: reset → admit/validate → gate → inject →
// contender → receipt. Failures and denials are preserved; no silent retries.
func (r *Runner) RunTrial(ctx context.Context, req TrialRequest) (AttemptReceipt, error) {
	receipt := AttemptReceipt{
		ClaimsEfficacy: false,
		Usage: UsageRecord{
			Unknowns:       []string{},
			AccountingNote: "synthetic/local runs report zero spend unless a future authorized adapter records otherwise",
		},
		Isolation: IsolationReport{
			NamedLimitations: append([]string(nil), r.isolationLimits...),
		},
		OutputRefs: []string{},
	}

	attempt := contract.NewAttemptDocument()
	attempt.StartedAt = time.Now().UTC().Format(time.RFC3339)
	attempt.AttemptID = "attempt-" + sanitizeID(req.Case.CaseID) + "-" + sanitizeID(req.Condition.ConditionID) + "-" + itoa(req.RepIndex)
	attempt.ConditionID = req.Condition.ConditionID
	attempt.CaseID = req.Case.CaseID
	attempt.RepIndex = req.RepIndex
	attempt.TimeoutSeconds = req.Gate.Policy.DefaultTimeoutSeconds
	if req.TimeoutSeconds > 0 {
		attempt.TimeoutSeconds = req.TimeoutSeconds
	}
	if attempt.TimeoutSeconds <= 0 {
		attempt.TimeoutSeconds = 60
	}

	contender := req.Contender
	if contender == nil {
		contender = &SyntheticContender{}
	}
	class := contender.Class()

	// Gate check BEFORE any provider / contender start for external class.
	decision := EvaluateGate(req.Gate, class, r.budget)
	if !decision.Allowed {
		attempt.Status = contract.AttemptSkipped
		if decision.ErrorCode == CodeBudgetExhausted {
			attempt.Status = contract.AttemptSkipped
		}
		if decision.ErrorCode == CodeUnavailable {
			attempt.Status = contract.AttemptUnavailable
		}
		attempt.ProviderInvoked = false
		attempt.SkipReason = decision.Reason
		attempt.ErrorCode = decision.ErrorCode
		attempt.ResetObserved = false
		attempt.IsolationHeld = true
		attempt.FinishedAt = time.Now().UTC().Format(time.RFC3339)
		receipt.Attempt = attempt
		receipt.ProviderStarted = false
		receipt.RunnerNotes = "terminated before provider execution"
		_ = r.writeReceipt(receipt)
		return receipt, errf(decision.ErrorCode, "gate", "%s", decision.Reason)
	}

	// Reset and verify clean hashes.
	if !req.SkipReset {
		hashes, err := r.ws.Reset()
		if err != nil {
			attempt.Status = contract.AttemptFailed
			attempt.ErrorCode = CodeResetBleed
			attempt.ProviderInvoked = false
			attempt.ResetObserved = false
			attempt.IsolationHeld = false
			attempt.FinishedAt = time.Now().UTC().Format(time.RFC3339)
			receipt.Attempt = attempt
			receipt.Isolation.ResetObserved = false
			receipt.Isolation.InitialHashesMatched = false
			_ = r.writeReceipt(receipt)
			return receipt, err
		}
		receipt.Isolation.ResetObserved = true
		receipt.Isolation.InitialHashesMatched = hashes == r.ws.Baseline()
		attempt.ResetObserved = true
	} else if err := r.ws.VerifyCleanInitial(); err != nil {
		receipt.Isolation.InitialHashesMatched = false
	} else {
		receipt.Isolation.InitialHashesMatched = true
		attempt.ResetObserved = true
		receipt.Isolation.ResetObserved = true
	}

	// Re-validate case (admission path for in-memory synthetic).
	adm, err := r.AdmitCasePackage(AdmitOptions{Case: &req.Case, AllowSynthetic: true})
	if err != nil {
		attempt.Status = contract.AttemptFailed
		attempt.ErrorCode = CodeAdmitReject
		attempt.ProviderInvoked = false
		attempt.IsolationHeld = true
		attempt.FinishedAt = time.Now().UTC().Format(time.RFC3339)
		receipt.Attempt = attempt
		_ = r.writeReceipt(receipt)
		return receipt, err
	}
	_ = adm

	exp, err := r.InjectCondition(req.Case, req.Condition)
	if err != nil {
		attempt.Status = contract.AttemptFailed
		attempt.ErrorCode = codeOf(err)
		attempt.ProviderInvoked = false
		attempt.IsolationHeld = true
		attempt.FinishedAt = time.Now().UTC().Format(time.RFC3339)
		receipt.Attempt = attempt
		receipt.Exposure = exp
		_ = r.writeReceipt(receipt)
		return receipt, err
	}
	receipt.Exposure = exp

	receipt.Digests = InputDigests{
		CaseID:            req.Case.CaseID,
		ConditionID:       req.Condition.ConditionID,
		RepoCommitSHA:     req.Case.Repository.CommitSHA,
		WorkspaceBaseline: r.ws.Baseline(),
		Exposure:          exp.DeliveredDigest,
		Snapshot:          exp.SnapshotBytesSHA256,
		HarnessID:         req.Case.Harness.HarnessID,
		ModelID:           req.Case.Harness.ModelID,
	}

	// Isolation pre-check: no oracle in contender before run.
	if leak, paths, lerr := r.ws.ContenderHasOracleMaterial(); lerr != nil {
		return receipt, lerr
	} else if leak {
		attempt.Status = contract.AttemptFailed
		attempt.ErrorCode = CodeOracleLeak
		attempt.ProviderInvoked = false
		attempt.IsolationHeld = false
		attempt.FinishedAt = time.Now().UTC().Format(time.RFC3339)
		receipt.Attempt = attempt
		receipt.Isolation.OracleLeakDetected = true
		receipt.Isolation.OracleLeakPaths = paths
		_ = r.writeReceipt(receipt)
		return receipt, errf(CodeOracleLeak, "contender", "oracle material present before run: %v", paths)
	}

	started := time.Now().UTC()
	attempt.Status = contract.AttemptRunning

	timeout := TimeoutFor(req.Gate, req.TimeoutSeconds)
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	providerStarted := class == ContenderExternalProvider
	if providerStarted {
		// Should be unreachable: EvaluateGate denies external providers.
		receipt.ProviderStarted = true
		attempt.ProviderInvoked = true
	}

	out, runErr := contender.Run(runCtx, ContenderInput{
		CaseID:        req.Case.CaseID,
		Role:          req.Case.Role,
		ConditionKind: req.Condition.Kind,
		RepIndex:      req.RepIndex,
		Workspace:     r.ws.Layout,
		Exposure:      exp,
		Harness:       req.Case.Harness,
		Timeout:       timeout,
	})
	finished := time.Now().UTC()
	attempt.FinishedAt = finished.Format(time.RFC3339)

	receipt.ExitClass = out.ExitClass
	receipt.OutputRefs = append([]string{}, out.ArtifactRefs...)
	if out.StdoutRef != "" {
		receipt.OutputRefs = append(receipt.OutputRefs, out.StdoutRef)
	}
	if out.StderrRef != "" {
		receipt.OutputRefs = append(receipt.OutputRefs, out.StderrRef)
	}
	receipt.Usage.TokensObserved = out.UsageTokens
	receipt.Usage.CostUSDMicros = out.CostUSDMicros
	receipt.Usage.CostKnown = true
	receipt.Usage.Unknowns = append([]string{}, out.Unknowns...)

	writes, _ := r.ws.ListContenderWrites()
	receipt.Isolation.ContenderWrites = writes
	if leak, paths, _ := r.ws.ContenderHasOracleMaterial(); leak {
		receipt.Isolation.OracleLeakDetected = true
		receipt.Isolation.OracleLeakPaths = paths
		attempt.IsolationHeld = false
	} else {
		attempt.IsolationHeld = true
	}

	switch {
	case runCtx.Err() == context.DeadlineExceeded || out.ExitClass == "timeout":
		attempt.Status = contract.AttemptTimedOut
		attempt.ErrorCode = CodeTimeout
		attempt.ProviderInvoked = providerStarted
	case out.ExitClass == "unavailable":
		attempt.Status = contract.AttemptUnavailable
		attempt.ErrorCode = CodeUnavailable
		attempt.ProviderInvoked = false
		attempt.SkipReason = out.Notes
	case out.ExitClass == "malformed":
		attempt.Status = contract.AttemptFailed
		attempt.ErrorCode = CodeMalformedOutput
		attempt.ProviderInvoked = providerStarted
	case out.ExitClass == "task_failure":
		// Task failure is a completed attempt with failed harness exit —
		// preserved distinctly; runner does not score efficacy.
		attempt.Status = contract.AttemptFailed
		attempt.ErrorCode = "task_failure"
		attempt.ProviderInvoked = providerStarted
	case runErr != nil:
		attempt.Status = contract.AttemptFailed
		attempt.ErrorCode = codeOf(runErr)
		attempt.ProviderInvoked = providerStarted
		attempt.Notes = runErr.Error()
	case out.ExitClass == "ok":
		attempt.Status = contract.AttemptSucceeded
		attempt.ProviderInvoked = providerStarted
	default:
		attempt.Status = contract.AttemptFailed
		attempt.ErrorCode = CodeInternal
		attempt.ProviderInvoked = providerStarted
	}

	attempt.CostUSDMicros = out.CostUSDMicros
	receipt.Attempt = attempt
	receipt.ClaimsEfficacy = false
	receipt.RunnerNotes = "runner receipt establishes infrastructure readiness only; not a historical efficacy claim"

	r.budget.RunsUsed++
	r.budget.SpentUSDMicros += out.CostUSDMicros
	r.budget.WallSecondsUsed += int(finished.Sub(started).Seconds())
	r.receipts = append(r.receipts, receipt)
	_ = r.writeReceipt(receipt)

	// Preserve failure: return runErr if any, but receipt is still written.
	if runErr != nil && attempt.Status != contract.AttemptTimedOut {
		return receipt, runErr
	}
	return receipt, nil
}

func codeOf(err error) string {
	if e, ok := err.(*Error); ok {
		return e.Code
	}
	return CodeInternal
}

func (r *Runner) writeReceipt(receipt AttemptReceipt) error {
	dir := filepath.Join(r.ws.Layout.Artifacts, "receipts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	name := sanitizeID(receipt.Attempt.AttemptID) + ".json"
	b, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name), b, 0o644)
}
