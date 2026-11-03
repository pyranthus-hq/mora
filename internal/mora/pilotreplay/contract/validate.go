package contract

import (
	"fmt"
	"time"
)

// Validate checks CaseDocument invariants, including runner/oracle separation
// and memory-snapshot vs delivered-context separation.
func (c *CaseDocument) Validate() error {
	if err := c.validate(SchemaCase); err != nil {
		return err
	}
	if err := validateID("case_id", c.CaseID); err != nil {
		return err
	}
	if err := inSet("role", c.Role,
		CaseRoleFailure, CaseRoleValidMemoryControl, CaseRoleNoRelevantMemoryCtrl); err != nil {
		return err
	}
	if err := c.Eligibility.Validate("eligibility"); err != nil {
		return err
	}
	if err := c.Cutoff.Validate("cutoff"); err != nil {
		return err
	}
	if err := c.Repository.Validate("repository"); err != nil {
		return err
	}
	if err := c.Harness.Validate("harness"); err != nil {
		return err
	}
	if err := c.MemorySnapshot.Validate("memory_snapshot"); err != nil {
		return err
	}
	if err := c.DeliveredContext.Validate("delivered_context"); err != nil {
		return err
	}
	if err := rejectSilentFidelityUpgrade(
		"delivered_context.fidelity",
		c.MemorySnapshot.Fidelity,
		c.DeliveredContext.Fidelity,
	); err != nil {
		return err
	}
	if err := c.RunnerPackage.Validate("runner_package"); err != nil {
		return err
	}
	if err := c.OraclePackage.Validate("oracle_package"); err != nil {
		return err
	}
	if err := c.Controls.Validate("controls", c.Role); err != nil {
		return err
	}
	if c.IsolationAssumptions == nil {
		return errf(CodeMissingField, "isolation_assumptions", "use [] when empty, never null")
	}
	if err := validateNote("notes", c.Notes); err != nil {
		return err
	}
	return nil
}

// Validate checks Eligibility.
func (e *Eligibility) Validate(field string) error {
	if err := inSet(field+".decision", e.Decision, "eligible", "no_go", "prospective_capture"); err != nil {
		return err
	}
	if e.MissingInputs == nil {
		return errf(CodeMissingField, field+".missing_inputs", "use [] when empty, never null")
	}
	if e.Decision == "eligible" {
		if !e.PermissionRecorded {
			return errf(CodePermissionMissing, field+".permission_recorded", "eligible cases require a recorded permission")
		}
		if !e.PrivatePackageOutsidePublicRepo {
			return errf(CodeInvalidValue, field+".private_package_outside_public_repo", "eligible cases must keep the private package outside the public repo")
		}
	}
	if e.EligibleForHistoricalAttribution && e.Decision != "eligible" {
		return errf(CodeInvalidValue, field+".eligible_for_historical_attribution", "historical attribution requires decision=eligible")
	}
	return validateNote(field+".rationale", e.Rationale)
}

// Validate checks TimeCutoff.
func (t *TimeCutoff) Validate(field string) error {
	if err := validateTimestamp(field+".cutoff_at", t.CutoffAt, true); err != nil {
		return err
	}
	if !t.PostCutoffForbidden {
		return errf(CodeIsolationBreach, field+".post_cutoff_forbidden_to_contender", "post-cutoff evidence must be forbidden to the contender")
	}
	return validateNote(field+".timezone_note", t.TimezoneNote)
}

// Validate checks RepositoryState.
func (r *RepositoryState) Validate(field string) error {
	if err := validateCommitSHA(field+".commit_sha", r.CommitSHA); err != nil {
		return err
	}
	if r.DirtyFiles == nil {
		return errf(CodeMissingField, field+".dirty_files", "use [] when empty, never null")
	}
	if len(r.DirtyFiles) > MaxDirtyFiles {
		return errf(CodeTooManyItems, field+".dirty_files", "exceeds %d", MaxDirtyFiles)
	}
	for i, d := range r.DirtyFiles {
		prefix := fmt.Sprintf("%s.dirty_files[%d]", field, i)
		if err := validatePath(prefix+".path", d.Path, true); err != nil {
			return err
		}
		if err := validateHash(prefix+".content_sha256", d.ContentSHA, true); err != nil {
			return err
		}
		if err := inSet(prefix+".status", d.Status, "modified", "added", "deleted", "renamed"); err != nil {
			return err
		}
	}
	return validateNote(field+".reconstruction_note", r.ReconstructionNote)
}

// Validate checks HarnessConfig.
func (h *HarnessConfig) Validate(field string) error {
	if err := validateLabel(field+".model_id", h.ModelID, true); err != nil {
		return err
	}
	if err := validateLabel(field+".provider", h.Provider, true); err != nil {
		return err
	}
	if err := inSet(field+".provider_version_status", h.ProviderVersionStatus,
		ProviderVersionKnown, ProviderVersionMissing, ProviderVersionApprox); err != nil {
		return err
	}
	if h.ProviderVersionStatus == ProviderVersionKnown && h.ProviderVersion == "" {
		return errf(CodeMissingField, field+".provider_version", "required when status is known")
	}
	if h.ProviderVersionStatus != ProviderVersionKnown && h.ProviderVersion != "" {
		// Approximate may carry a note-like version string; missing must not.
		if h.ProviderVersionStatus == ProviderVersionMissing {
			return errf(CodeInvalidValue, field+".provider_version", "must be empty when status is missing")
		}
	}
	if err := validateLabel(field+".toolset_id", h.ToolsetID, true); err != nil {
		return err
	}
	if err := validateLabel(field+".harness_id", h.HarnessID, true); err != nil {
		return err
	}
	if err := validateLabel(field+".harness_version", h.HarnessVersion, true); err != nil {
		return err
	}
	return validateNote(field+".notes", h.Notes)
}

// Validate checks MemorySnapshot.
func (m *MemorySnapshot) Validate(field string) error {
	if err := validateID(field+".snapshot_id", m.SnapshotID); err != nil {
		return err
	}
	if err := validateLabel(field+".format", m.Format, true); err != nil {
		return err
	}
	if err := validateHash(field+".bytes_sha256", m.BytesSHA256, true); err != nil {
		return err
	}
	if m.ByteLength < 0 {
		return errf(CodeInvalidValue, field+".byte_length", "cannot be negative")
	}
	if err := validateLabel(field+".memory_version", m.MemoryVersion, true); err != nil {
		return err
	}
	if m.RecordIDs == nil {
		return errf(CodeMissingField, field+".record_ids", "use [] when empty, never null")
	}
	if len(m.RecordIDs) > MaxSnapshotRefs {
		return errf(CodeTooManyItems, field+".record_ids", "exceeds %d", MaxSnapshotRefs)
	}
	if err := inSet(field+".fidelity", m.Fidelity,
		FidelityFaithfulHistorical, FidelityReconstruction, FidelityPartial, FidelityUnknown); err != nil {
		return err
	}
	if m.Fidelity == FidelityFaithfulHistorical && m.Synthetic {
		return errf(CodeFidelityUpgrade, field+".fidelity", "synthetic snapshots cannot claim faithful_historical")
	}
	return validateNote(field+".notes", m.Notes)
}

// Validate checks DeliveredContext. Snapshot bytes and exposure bytes stay distinct.
func (d *DeliveredContext) Validate(field string) error {
	if err := validateID(field+".manifest_id", d.ManifestID); err != nil {
		return err
	}
	if err := inSet(field+".exposure_availability", d.ExposureAvailability,
		ExposurePresent, ExposureMissing, ExposurePartial, ExposureSynthetic); err != nil {
		return err
	}
	if err := inSet(field+".fidelity", d.Fidelity,
		FidelityFaithfulHistorical, FidelityReconstruction, FidelityPartial, FidelityUnknown); err != nil {
		return err
	}
	if d.Parts == nil {
		return errf(CodeMissingField, field+".parts", "use [] when empty, never null")
	}
	if len(d.Parts) > MaxDeliveredParts {
		return errf(CodeTooManyItems, field+".parts", "exceeds %d", MaxDeliveredParts)
	}
	switch d.ExposureAvailability {
	case ExposurePresent:
		if len(d.Parts) == 0 {
			return errf(CodeMissingField, field+".parts", "present exposure requires at least one part")
		}
		if d.Fidelity == FidelityUnknown {
			return errf(CodeInvalidValue, field+".fidelity", "present exposure cannot be fidelity=unknown")
		}
	case ExposurePartial:
		if len(d.Parts) == 0 {
			return errf(CodeMissingField, field+".parts", "partial exposure requires at least one part")
		}
		if d.MissingReason == "" {
			return errf(CodeMissingField, field+".missing_reason", "required for the missing remainder of partial exposure")
		}
		if d.Fidelity == FidelityFaithfulHistorical {
			return errf(CodeFidelityUpgrade, field+".fidelity", "partial exposure cannot claim faithful_historical")
		}
	case ExposureMissing:
		if len(d.Parts) != 0 {
			return errf(CodeExposureMismatch, field+".parts", "missing exposure must have empty parts")
		}
		if d.MissingReason == "" {
			return errf(CodeMissingField, field+".missing_reason", "required when exposure is missing")
		}
		if d.Fidelity == FidelityFaithfulHistorical {
			return errf(CodeFidelityUpgrade, field+".fidelity", "missing exposure cannot claim faithful_historical")
		}
	case ExposureSynthetic:
		if !d.Synthetic {
			return errf(CodeInvalidValue, field+".synthetic", "synthetic exposure requires synthetic=true")
		}
		if d.Fidelity == FidelityFaithfulHistorical {
			return errf(CodeFidelityUpgrade, field+".fidelity", "synthetic exposure cannot claim faithful_historical")
		}
	}
	for i, p := range d.Parts {
		prefix := fmt.Sprintf("%s.parts[%d]", field, i)
		if err := validateID(prefix+".part_id", p.PartID); err != nil {
			return err
		}
		if err := inSet(prefix+".kind", p.Kind, "system", "user", "tool_result", "memory_block", "other"); err != nil {
			return err
		}
		if err := validateHash(prefix+".bytes_sha256", p.BytesSHA256, true); err != nil {
			return err
		}
		if p.ByteLength < 0 {
			return errf(CodeInvalidValue, prefix+".byte_length", "cannot be negative")
		}
	}
	return validateNote(field+".missing_reason", d.MissingReason)
}

// rejectSilentFidelityUpgrade forbids promoting a reconstructed snapshot to
// faithful historical exposure. Partial exports may have independently exact logs.
func rejectSilentFidelityUpgrade(field, snapshotFid, exposureFid string) error {
	if snapshotFid == FidelityReconstruction && exposureFid == FidelityFaithfulHistorical {
		return errf(CodeFidelityUpgrade, field, "do not silently upgrade reconstruction to faithful_historical")
	}
	return nil
}

// Validate checks RunnerPackage. Hidden oracle material in the runner fails closed.
func (r *RunnerPackage) Validate(field string) error {
	if err := validateID(field+".package_id", r.PackageID); err != nil {
		return err
	}
	if r.ContenderVisiblePaths == nil {
		return errf(CodeMissingField, field+".contender_visible_paths", "use [] when empty, never null")
	}
	for i, p := range r.ContenderVisiblePaths {
		if err := validatePath(fmt.Sprintf("%s.contender_visible_paths[%d]", field, i), p, true); err != nil {
			return err
		}
	}
	if r.ContainsHiddenTests || r.ContainsExpectedAnswers || r.ContainsReferencePatch || r.ContainsPostCutoff {
		return errf(CodeHiddenInRunner, field, "runner package must not contain hidden tests, expected answers, reference patches, or post-cutoff evidence")
	}
	if !r.ContentMinimized {
		return errf(CodeInvalidValue, field+".content_minimized", "runner package must declare content minimization")
	}
	return nil
}

// Validate checks OraclePackage.
func (o *OraclePackage) Validate(field string) error {
	if err := validateID(field+".package_id", o.PackageID); err != nil {
		return err
	}
	if o.HiddenTestRefs == nil || o.ExpectedAnswerRefs == nil || o.ReferencePatchRefs == nil || o.PostCutoffEvidenceRefs == nil {
		return errf(CodeMissingField, field, "oracle ref slices must be non-null ([] when empty)")
	}
	if o.ReferenceMemoryCeiling {
		return errf(CodeInvalidValue, field+".reference_memory_ceiling", "reference-memory ceiling is future optional work, not an initial condition")
	}
	if !o.InaccessibleToContender || !o.InaccessibleToRepair {
		return errf(CodeIsolationBreach, field, "oracle package must be inaccessible to contender and repair")
	}
	return nil
}

// Validate checks ControlSpec against case role.
func (c *ControlSpec) Validate(field, role string) error {
	if err := validateNote(field+".valid_memory_constraint", c.ValidMemoryConstraint); err != nil {
		return err
	}
	if err := validateNote(field+".no_relevant_memory_success_rule", c.NoRelevantMemorySuccessRule); err != nil {
		return err
	}
	switch role {
	case CaseRoleFailure:
		if c.ValidMemoryConstraint == "" || !c.ValidMemoryMustSurvive {
			return errf(CodeMissingField, field, "failure case must specify a valid memory constraint that must survive")
		}
		if c.NoRelevantMemorySuccessRule == "" || !c.NoRelevantMemoryTask {
			return errf(CodeMissingField, field, "failure case must specify the no-relevant-memory control rule")
		}
	case CaseRoleValidMemoryControl:
		if c.ValidMemoryConstraint == "" || !c.ValidMemoryMustSurvive {
			return errf(CodeMissingField, field, "valid-memory control must state the surviving constraint")
		}
	case CaseRoleNoRelevantMemoryCtrl:
		if !c.NoRelevantMemoryTask || c.NoRelevantMemorySuccessRule == "" {
			return errf(CodeMissingField, field, "no-relevant-memory control must state that success must not require the edit")
		}
	}
	return nil
}

// Validate checks ConditionDocument.
func (c *ConditionDocument) Validate() error {
	if err := c.validate(SchemaCondition); err != nil {
		return err
	}
	if err := validateID("condition_id", c.ConditionID); err != nil {
		return err
	}
	if err := validateID("case_id", c.CaseID); err != nil {
		return err
	}
	if err := inSet("kind", c.Kind,
		ConditionOriginalMemory, ConditionNoMemory, ConditionReviewedMemoryEdit); err != nil {
		return err
	}
	if c.DeclaredConfounds == nil {
		return errf(CodeMissingField, "declared_confounds", "use [] when empty, never null")
	}
	if len(c.DeclaredConfounds) > MaxConfoundNotes {
		return errf(CodeTooManyItems, "declared_confounds", "exceeds %d", MaxConfoundNotes)
	}
	if c.Kind == ConditionReviewedMemoryEdit {
		if c.MemoryEditRef == "" {
			return errf(CodeMissingField, "memory_edit_ref", "required for reviewed_memory_edit")
		}
		if c.EditReviewedBy == "" {
			return errf(CodeMissingField, "edit_reviewed_by", "reviewed edit requires a reviewer identity (synthetic ok)")
		}
	}
	if (!c.HarnessUnchanged || !c.RetrievalUnchanged) && len(c.DeclaredConfounds) == 0 {
		return errf(CodeInvalidValue, "declared_confounds", "changing harness or retrieval requires a declared confound or a separate condition")
	}
	return validateNote("notes", c.Notes)
}

// Validate checks AttemptDocument. Status vocabulary distinguishes terminals.
func (a *AttemptDocument) Validate() error {
	if err := a.validate(SchemaAttempt); err != nil {
		return err
	}
	if err := validateID("attempt_id", a.AttemptID); err != nil {
		return err
	}
	if err := validateID("condition_id", a.ConditionID); err != nil {
		return err
	}
	if err := validateID("case_id", a.CaseID); err != nil {
		return err
	}
	if a.RepIndex < 0 {
		return errf(CodeInvalidValue, "rep_index", "cannot be negative")
	}
	if err := inSet("status", a.Status,
		AttemptPending, AttemptRunning, AttemptSucceeded, AttemptFailed,
		AttemptTimedOut, AttemptSkipped, AttemptUnavailable); err != nil {
		return err
	}
	if err := validateTimestamp("started_at", a.StartedAt, false); err != nil {
		return err
	}
	terminal := a.Status != AttemptPending && a.Status != AttemptRunning
	if err := validateTimestamp("finished_at", a.FinishedAt, terminal); err != nil {
		return err
	}
	if a.StartedAt != "" && a.FinishedAt != "" {
		started, _ := time.Parse(time.RFC3339, a.StartedAt)
		finished, _ := time.Parse(time.RFC3339, a.FinishedAt)
		if finished.Before(started) {
			return errf(CodeInvalidValue, "finished_at", "must not precede started_at")
		}
	}
	if a.TimeoutSeconds <= 0 {
		return errf(CodeInvalidValue, "timeout_seconds", "must be positive")
	}
	if a.CostUSDMicros < 0 {
		return errf(CodeInvalidValue, "cost_usd_micros", "cannot be negative")
	}
	switch a.Status {
	case AttemptSkipped, AttemptUnavailable:
		if a.CostUSDMicros != 0 {
			return errf(CodeInvalidValue, "cost_usd_micros", "skipped/unavailable attempts must have zero cost")
		}
		if a.SkipReason == "" {
			return errf(CodeMissingField, "skip_reason", "required for skipped/unavailable attempts")
		}
		if a.ProviderInvoked {
			return errf(CodeInvalidValue, "provider_invoked", "skipped/unavailable attempts must not invoke the provider")
		}
	case AttemptSucceeded:
		if a.ErrorCode != "" {
			return errf(CodeInvalidValue, "error_code", "successful attempts must not carry an error code")
		}
		if !a.ProviderInvoked && a.SkipReason != "" {
			return errf(CodeInvalidValue, "status", "succeeded attempts are not skips")
		}
	case AttemptFailed, AttemptTimedOut:
		if a.ErrorCode == "" {
			return errf(CodeMissingField, "error_code", "required for failed/timed_out attempts")
		}
	}
	if a.Status == AttemptSucceeded && !a.ResetObserved {
		return errf(CodeInvalidValue, "reset_observed", "successful attempts must record workspace reset")
	}
	if a.Status == AttemptSucceeded && !a.IsolationHeld {
		return errf(CodeIsolationBreach, "isolation_held", "successful attempts must affirm isolation held")
	}
	return validateNote("notes", a.Notes)
}

// Validate checks OutcomeDocument. Successful outcomes are distinct from skip/timeout/fail.
func (o *OutcomeDocument) Validate() error {
	if err := o.validate(SchemaOutcome); err != nil {
		return err
	}
	if err := validateID("outcome_id", o.OutcomeID); err != nil {
		return err
	}
	if err := validateID("attempt_id", o.AttemptID); err != nil {
		return err
	}
	if err := inSet("kind", o.Kind,
		OutcomePass, OutcomeFail, OutcomeError, OutcomeSkipped,
		OutcomeUnavailable, OutcomeTimedOut, OutcomeInconclusive); err != nil {
		return err
	}
	if err := validateLabel("checker_id", o.CheckerID, true); err != nil {
		return err
	}
	return validateNote("notes", o.Notes)
}

// Validate checks that attempt status and outcome kind are consistent.
func ValidateAttemptOutcomePair(a AttemptDocument, o OutcomeDocument) error {
	if a.AttemptID != o.AttemptID {
		return errf(CodeInvalidValue, "attempt_id", "outcome attempt_id must match attempt")
	}
	switch a.Status {
	case AttemptSucceeded:
		if a.ErrorCode != "" {
			return errf(CodeInvalidValue, "error_code", "successful attempts must not carry an error code")
		}
		if o.Kind != OutcomePass && o.Kind != OutcomeFail && o.Kind != OutcomeInconclusive {
			return errf(CodeInvalidValue, "outcome.kind", "succeeded attempt cannot pair with %s", o.Kind)
		}
	case AttemptFailed:
		if o.Kind != OutcomeFail && o.Kind != OutcomeError {
			return errf(CodeInvalidValue, "outcome.kind", "failed attempt pairs with fail/error, got %s", o.Kind)
		}
	case AttemptTimedOut:
		if o.Kind != OutcomeTimedOut {
			return errf(CodeInvalidValue, "outcome.kind", "timed_out attempt requires timed_out outcome")
		}
	case AttemptSkipped:
		if o.Kind != OutcomeSkipped {
			return errf(CodeInvalidValue, "outcome.kind", "skipped attempt requires skipped outcome")
		}
	case AttemptUnavailable:
		if o.Kind != OutcomeUnavailable {
			return errf(CodeInvalidValue, "outcome.kind", "unavailable attempt requires unavailable outcome")
		}
	}
	return nil
}

// Validate checks ExecutionPolicy.
func (p *ExecutionPolicy) Validate(field string) error {
	if !p.ResetRequired || !p.IsolationRequired || !p.ContentMinimization {
		return errf(CodeInvalidValue, field, "reset, isolation and content minimization are required")
	}
	if p.DefaultTimeoutSeconds <= 0 {
		return errf(CodeInvalidValue, field+".default_timeout_seconds", "must be positive")
	}
	if err := inSet(field+".retry.missing_run_policy", p.Retry.MissingRunPolicy,
		"record_unavailable", "exclude_with_note", "fail_closed"); err != nil {
		return err
	}
	if p.Retry.MaxRetriesPerAttempt < 0 {
		return errf(CodeInvalidValue, field+".retry.max_retries_per_attempt", "cannot be negative")
	}
	if err := p.Cost.Validate(field + ".cost"); err != nil {
		return err
	}
	if p.DefaultDeny == nil {
		return errf(CodeMissingField, field+".default_deny", "use [] when empty, never null")
	}
	need := map[string]bool{
		DenyProductionWrites: false,
		DenyExternalActions:  false,
		DenyLiveVaultAccess:  false,
	}
	for _, d := range p.DefaultDeny {
		if _, ok := need[d]; ok {
			need[d] = true
		}
	}
	for k, ok := range need {
		if !ok {
			return errf(CodeInvalidValue, field+".default_deny", "missing required deny %q", k)
		}
	}
	return nil
}

// Validate checks CostAccounting. Fail-closed spend ceiling is mandatory.
func (c *CostAccounting) Validate(field string) error {
	if err := inSet(field+".currency", c.Currency, "USD"); err != nil {
		return err
	}
	if c.SpentUSDMicros < 0 {
		return errf(CodeInvalidValue, field+".spent_usd_micros", "cannot be negative")
	}
	if c.CeilingUSDMicros <= 0 {
		return errf(CodeSpendCeiling, field+".ceiling_usd_micros", "fail-closed spend ceiling is required before provider invocation")
	}
	if !c.FailClosed {
		return errf(CodeSpendCeiling, field+".fail_closed", "spend ceiling must be fail-closed")
	}
	if c.SpentUSDMicros > c.CeilingUSDMicros {
		return errf(CodeSpendCeiling, field+".spent_usd_micros", "spent exceeds ceiling")
	}
	if c.ProviderCalls < 0 {
		return errf(CodeInvalidValue, field+".provider_calls", "cannot be negative")
	}
	return nil
}

// Validate checks RunGate. Rejects absent permission or spend ceiling before
// any provider invocation is authorized.
func (g *RunGate) Validate() error {
	if err := g.validate(SchemaRunGate); err != nil {
		return err
	}
	if err := validateID("gate_id", g.GateID); err != nil {
		return err
	}
	if err := g.Policy.Validate("policy"); err != nil {
		return err
	}
	return validateNote("notes", g.Notes)
}

// AuthorizeProviderInvocation returns nil only when permission, frozen matrix
// and monetary ceiling are present. Callers must invoke this before any
// provider call; schema tests assert rejection when any gate is absent.
func (g *RunGate) AuthorizeProviderInvocation() error {
	if err := g.Validate(); err != nil {
		return err
	}
	if !g.RunPermissionGranted {
		return errf(CodePermissionMissing, "run_permission_granted", "reject provider invocation without run permission")
	}
	if g.PermissionRecordRef == "" {
		return errf(CodePermissionMissing, "permission_record_ref", "permission record ref required when granting runs")
	}
	if !g.MatrixFrozen {
		return errf(CodeRunGateDenied, "matrix_frozen", "actual matrix must freeze before model execution")
	}
	if !g.MonetaryCeilingFrozen {
		return errf(CodeRunGateDenied, "monetary_ceiling_frozen", "monetary ceiling must freeze before model execution")
	}
	if err := g.Policy.Cost.Validate("policy.cost"); err != nil {
		return err
	}
	return nil
}

// Validate checks PlanningMatrix. Default must not claim approved spend.
func (m *PlanningMatrix) Validate(field string) error {
	if m.RepsPerCell <= 0 || m.FailureCases < 0 || m.ControlCases < 0 || m.Conditions <= 0 {
		return errf(CodeInvalidValue, field, "matrix dimensions must be non-negative with positive reps and conditions")
	}
	want := m.RepsPerCell * (m.FailureCases + m.ControlCases) * m.Conditions
	if m.MaxPlannedRuns != want {
		return errf(CodeInvalidValue, field+".max_planned_runs", "want %d, got %d", want, m.MaxPlannedRuns)
	}
	if m.IsApprovedSpend {
		return errf(CodeInvalidValue, field+".is_approved_spend", "planning matrix must not claim approved spend")
	}
	if m.IsStatisticalClaim {
		return errf(CodeInvalidValue, field+".is_statistical_claim", "planning matrix must not claim statistical power")
	}
	if !m.MustFreezeBeforeExec {
		return errf(CodeInvalidValue, field+".must_freeze_before_exec", "matrix must declare freeze-before-exec")
	}
	return nil
}

// Validate checks AccessRow.
func (r *AccessRow) Validate(field string) error {
	if err := inSet(field+".principal", r.Principal,
		PrincipalContender, PrincipalRepair, PrincipalOracle, PrincipalRunner, PrincipalOperator); err != nil {
		return err
	}
	if err := validateLabel(field+".resource", r.Resource, true); err != nil {
		return err
	}
	if err := inSet(field+".access", r.Access, AccessDeny, AccessAllow, AccessRedact, AccessHashRef); err != nil {
		return err
	}
	return validateNote(field+".rationale", r.Rationale)
}

// Validate checks ReportDocument.
func (r *ReportDocument) Validate() error {
	if err := r.validate(SchemaReport); err != nil {
		return err
	}
	if err := validateID("report_id", r.ReportID); err != nil {
		return err
	}
	if r.CaseIDs == nil || r.ConditionIDs == nil || r.AttemptIDs == nil || r.OutcomeIDs == nil {
		return errf(CodeMissingField, "ids", "id slices must be non-null")
	}
	if err := r.Matrix.Validate("matrix"); err != nil {
		return err
	}
	if err := r.Cost.Validate("cost"); err != nil {
		return err
	}
	if r.AccessTable == nil {
		return errf(CodeMissingField, "access_table", "use [] when empty, never null")
	}
	if len(r.AccessTable) > MaxAccessRows {
		return errf(CodeTooManyItems, "access_table", "exceeds %d", MaxAccessRows)
	}
	for i := range r.AccessTable {
		if err := r.AccessTable[i].Validate(fmt.Sprintf("access_table[%d]", i)); err != nil {
			return err
		}
	}
	if r.Assumptions == nil || r.Limitations == nil {
		return errf(CodeMissingField, "assumptions", "assumption/limitation slices must be non-null")
	}
	if r.ClaimsEfficacy {
		return errf(CodeInvalidValue, "claims_efficacy", "contract reports must not claim downstream efficacy")
	}
	return validateNote("notes", r.Notes)
}

// Validate checks ContractReceipt.
func (r *ContractReceipt) Validate() error {
	if err := r.validate(SchemaReceipt); err != nil {
		return err
	}
	if err := validateID("receipt_id", r.ReceiptID); err != nil {
		return err
	}
	if err := validateTimestamp("generated_at", r.GeneratedAt, true); err != nil {
		return err
	}
	if r.ExampleHashes == nil {
		return errf(CodeMissingField, "example_hashes", "use {} when empty, never null")
	}
	if r.UnresolvedLimits == nil {
		return errf(CodeMissingField, "unresolved_limits", "use [] when empty, never null")
	}
	if r.ClaimsEfficacy {
		return errf(CodeInvalidValue, "claims_efficacy", "contract receipt must not claim downstream efficacy")
	}
	return validateNote("notes", r.Notes)
}
