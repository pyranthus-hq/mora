package report

import "github.com/pyranthus-hq/mora/internal/mora/pilotreplay/contract"

// DamageGateResult is the outcome of checking whether a repair "wins" by
// dropping a valid-memory constraint. Such a win fails the damage gate.
type DamageGateResult struct {
	TrialID    string `json:"trial_id"`
	CaseRole   string `json:"case_role"`
	Condition  string `json:"condition_kind"`
	Failed     bool   `json:"failed"`
	Reason     string `json:"reason,omitempty"`
	Preserved  *bool  `json:"constraint_preserved"`
	TaskPassed *bool  `json:"task_success"`
}

// EvaluateDamageGate fails when an edited-memory (or valid-memory-control)
// trial drops a required valid constraint. A repair that appears to improve
// the task by discarding constraints fails this gate.
//
// Rules:
//   - reviewed_memory_edit + ConstraintPreserved==false → fail
//   - valid_memory_control + ConstraintPreserved==false → fail
//   - if ConstraintPreserved is nil (missing observation) → fail closed when
//     ValidMemoryMustSurvive is expected for that role/condition
//   - no_relevant_memory_control does not use this gate for constraint drop;
//     it uses the no-relevant-memory success rule instead
func EvaluateDamageGate(t TrialRecord) DamageGateResult {
	res := DamageGateResult{
		TrialID:    t.TrialID,
		CaseRole:   t.CaseRole,
		Condition:  t.ConditionKind,
		Preserved:  t.ConstraintPreserved,
		TaskPassed: t.TaskSuccess,
	}
	needsConstraint := t.ConditionKind == contract.ConditionReviewedMemoryEdit ||
		t.CaseRole == contract.CaseRoleValidMemoryControl
	if !needsConstraint {
		res.Reason = "damage gate not applicable for this role/condition"
		return res
	}
	if t.ConstraintPreserved == nil {
		res.Failed = true
		res.Reason = "constraint preservation unobserved; fail closed"
		return res
	}
	if !*t.ConstraintPreserved {
		res.Failed = true
		res.Reason = "valid-memory constraint dropped; repair must not win by discarding constraints"
		return res
	}
	res.Reason = "valid-memory constraint preserved"
	return res
}

// ApplyDamageGate stamps DamageGateFailed on the trial and returns the result.
func ApplyDamageGate(t *TrialRecord) DamageGateResult {
	res := EvaluateDamageGate(*t)
	t.DamageGateFailed = res.Failed
	if res.Failed {
		// Negatives stay visible.
		t.Visible = true
		if t.OutcomeKind == "" || t.OutcomeKind == contract.OutcomePass {
			t.OutcomeKind = contract.OutcomeFail
		}
	}
	return res
}
