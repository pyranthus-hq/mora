package oracle

import "github.com/pyranthus-hq/mora/internal/mora/pilotreplay/contract"

// DimensionResult is one graded dimension. Detail is evaluator-only and must
// not appear in AllowedFeedback.
type DimensionResult struct {
	Dimension string `json:"dimension"`
	Status    string `json:"status"`
	// Evidence is a marker name or hash-ref — never a gold body.
	Evidence string `json:"evidence,omitempty"`
	// Detail is evaluator-only diagnostic text.
	Detail string `json:"detail,omitempty"`
}

// GradeReport is the full evaluator-side judgment for one artifact.
type GradeReport struct {
	Schema           string            `json:"schema"`
	SchemaVersion    int               `json:"schema_version"`
	ReportID         string            `json:"report_id"`
	AttemptID        string            `json:"attempt_id,omitempty"`
	ArtifactID       string            `json:"artifact_id,omitempty"`
	ArtifactDigest   string            `json:"artifact_digest,omitempty"`
	CheckerID        string            `json:"checker_id"`
	CheckerVersion   string            `json:"checker_version"`
	CheckerDigest    string            `json:"checker_digest"`
	PolicyID         string            `json:"policy_id"`
	PolicyDigest     string            `json:"policy_digest"`
	HiddenSpecDigest string            `json:"hidden_spec_digest"`
	Dimensions       []DimensionResult `json:"dimensions"`
	// OutcomeKind maps to contract.OutcomeDocument.Kind.
	OutcomeKind string `json:"outcome_kind"`
	// Scored is false for absent/malformed/unsupported/grader-error paths.
	Scored bool `json:"scored"`
	// ClaimsEfficacy must remain false for this package.
	ClaimsEfficacy bool   `json:"claims_efficacy"`
	Notes          string `json:"notes,omitempty"`
}

// AllowedFeedback is the only feedback shape safe to surface toward contender
// or repair authoring. It omits Detail bodies and all gold.
type AllowedFeedback struct {
	OutcomeKind       string            `json:"outcome_kind"`
	DimensionStatuses map[string]string `json:"dimension_statuses"`
	CheckerID         string            `json:"checker_id"`
	CheckerVersion    string            `json:"checker_version"`
	CheckerDigest     string            `json:"checker_digest"`
	PolicyID          string            `json:"policy_id"`
	ArtifactDigest    string            `json:"artifact_digest,omitempty"`
	Scored            bool              `json:"scored"`
}

// ToAllowedFeedback strips evaluator-only fields.
func (g GradeReport) ToAllowedFeedback() AllowedFeedback {
	statuses := make(map[string]string, len(g.Dimensions))
	for _, d := range g.Dimensions {
		statuses[d.Dimension] = d.Status
	}
	return AllowedFeedback{
		OutcomeKind:       g.OutcomeKind,
		DimensionStatuses: statuses,
		CheckerID:         g.CheckerID,
		CheckerVersion:    g.CheckerVersion,
		CheckerDigest:     g.CheckerDigest,
		PolicyID:          g.PolicyID,
		ArtifactDigest:    g.ArtifactDigest,
		Scored:            g.Scored,
	}
}

// ToOutcomeDocument builds a contract OutcomeDocument from the grade.
func (g GradeReport) ToOutcomeDocument(outcomeID, attemptID string) contract.OutcomeDocument {
	o := contract.NewOutcomeDocument()
	o.OutcomeID = outcomeID
	o.AttemptID = attemptID
	o.Kind = g.OutcomeKind
	o.CheckerID = g.CheckerID
	o.DetailRef = "oracle://grade/" + g.ReportID // oracle-side ref only
	o.Notes = "oracle grade; claims_efficacy=false; sensitivity only"
	return o
}

// AggregateOutcome rolls dimension statuses into a contract outcome kind.
// Rules (frozen):
//   - any DimError → OutcomeError (never success)
//   - any DimFail → OutcomeFail
//   - all required dims DimPass (others not_applicable) → OutcomePass
//   - otherwise → OutcomeInconclusive (never success)
func AggregateOutcome(dims []DimensionResult) string {
	hasError := false
	hasFail := false
	hasInconclusive := false
	requiredPass := 0
	requiredTotal := 0
	for _, d := range dims {
		switch d.Status {
		case DimError:
			hasError = true
		case DimFail:
			hasFail = true
		case DimInconclusive:
			hasInconclusive = true
		case DimPass:
			if d.Dimension != "" {
				requiredPass++
				requiredTotal++
			}
		case DimNotApplicable:
			// ignored for pass rollup
		default:
			hasInconclusive = true
		}
		if d.Status == DimPass || d.Status == DimFail || d.Status == DimInconclusive || d.Status == DimError {
			if d.Status != DimPass {
				// already counted above for pass; for fail/error/inconclusive
				// ensure requiredTotal tracks scored dimensions that matter
			}
		}
	}
	// Recount required: every dimension that is not not_applicable.
	requiredPass = 0
	requiredTotal = 0
	for _, d := range dims {
		if d.Status == DimNotApplicable {
			continue
		}
		requiredTotal++
		if d.Status == DimPass {
			requiredPass++
		}
	}
	if hasError {
		return contract.OutcomeError
	}
	if hasFail {
		return contract.OutcomeFail
	}
	if hasInconclusive || requiredTotal == 0 {
		return contract.OutcomeInconclusive
	}
	if requiredPass == requiredTotal {
		return contract.OutcomePass
	}
	return contract.OutcomeInconclusive
}
