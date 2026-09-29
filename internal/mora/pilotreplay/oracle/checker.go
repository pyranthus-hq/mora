package oracle

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pyranthus-hq/mora/internal/mora/pilotreplay/contract"
)

// GradeRequest is the evaluator-side input. HiddenSpec is never passed through
// a contender-visible channel.
type GradeRequest struct {
	AttemptID string
	ReportID  string
	Artifact  *OutputArtifact // nil ⇒ missing artifact
	Hidden    HiddenSpec
	// ForceGraderError injects a grader failure (negative tests only).
	ForceGraderError bool
	// EvalParent is the parent directory for the temporary evaluator env.
	// Empty uses the system temp dir.
	EvalParent string
}

// Checker grades sanitized artifacts under a frozen scoring policy.
type Checker struct {
	Policy ScoringPolicy
	Freeze CheckerFreezeRecord
}

// NewChecker builds a checker frozen against the given hidden spec.
func NewChecker(hidden HiddenSpec) (*Checker, error) {
	if err := hidden.Validate(); err != nil {
		return nil, err
	}
	rec, err := FreezeChecker(hidden)
	if err != nil {
		return nil, err
	}
	return &Checker{
		Policy: FrozenScoringPolicy(),
		Freeze: rec,
	}, nil
}

// Grade runs the frozen checker over a copied/sanitized artifact in a separate
// evaluator environment. Unsupported / missing / malformed artifacts and
// grader errors never become agent success.
func (c *Checker) Grade(req GradeRequest) (GradeReport, error) {
	if c == nil {
		return GradeReport{}, errf(CodeInternal, "checker", "nil checker")
	}
	if err := req.Hidden.Validate(); err != nil {
		return GradeReport{}, err
	}
	digest := CheckerDigest(c.Freeze)
	base := GradeReport{
		Schema:           GradeSchema,
		SchemaVersion:    GradeVer,
		ReportID:         req.ReportID,
		AttemptID:        req.AttemptID,
		CheckerID:        c.Policy.CheckerID,
		CheckerVersion:   c.Policy.CheckerVersion,
		CheckerDigest:    digest,
		PolicyID:         c.Policy.PolicyID,
		PolicyDigest:     c.Freeze.PolicyDigest,
		HiddenSpecDigest: c.Freeze.HiddenSpecDigest,
		ClaimsEfficacy:   false,
	}
	if base.ReportID == "" {
		base.ReportID = "grade-unspecified"
	}

	if req.ForceGraderError {
		dims := inconclusiveAll("grader_error", "forced grader error for negative test")
		for i := range dims {
			dims[i].Status = DimError
		}
		base.Dimensions = dims
		base.OutcomeKind = contract.OutcomeError
		base.Scored = false
		base.Notes = "grader error → unscored; never agent success"
		return base, errf(CodeGraderError, "grader", "forced grader error")
	}

	if req.Artifact == nil {
		base.Dimensions = inconclusiveAll("missing_artifact", "artifact pointer is nil")
		base.OutcomeKind = contract.OutcomeInconclusive
		base.Scored = false
		base.Notes = "missing artifact → inconclusive; never agent success"
		return base, nil
	}

	if err := req.Artifact.ValidateShape(); err != nil {
		code := CodeMalformedArtifact
		if e, ok := err.(*Error); ok {
			code = e.Code
		}
		detail := err.Error()
		statusNote := "malformed/unsupported artifact → inconclusive; never agent success"
		dims := inconclusiveAll(code, detail)
		base.Dimensions = dims
		base.OutcomeKind = contract.OutcomeInconclusive
		base.Scored = false
		base.Notes = statusNote
		base.ArtifactID = req.Artifact.ArtifactID
		return base, nil
	}

	env, err := PrepareEvaluatorEnv(req.EvalParent, *req.Artifact)
	if err != nil {
		dims := errorAll("evaluator_env", err.Error())
		base.Dimensions = dims
		base.OutcomeKind = contract.OutcomeError
		base.Scored = false
		base.Notes = "evaluator env failure → error; never agent success"
		return base, err
	}
	defer func() { _ = env.Close() }()

	base.ArtifactID = env.Artifact.ArtifactID
	base.ArtifactDigest = env.ArtifactDig

	dims, err := c.scoreDimensions(env, req.Hidden)
	if err != nil {
		base.Dimensions = errorAll("score", err.Error())
		base.OutcomeKind = contract.OutcomeError
		base.Scored = false
		base.Notes = "grader error during scoring → error; never agent success"
		return base, errf(CodeGraderError, "score", "%v", err)
	}
	base.Dimensions = dims
	base.OutcomeKind = AggregateOutcome(dims)
	base.Scored = base.OutcomeKind == contract.OutcomePass || base.OutcomeKind == contract.OutcomeFail
	base.Notes = "synthetic oracle grade; sensitivity only; claims_efficacy=false"
	// Hard guard: OutcomePass requires every applicable dimension to pass.
	if base.OutcomeKind == contract.OutcomePass {
		for _, d := range dims {
			if d.Status == DimFail || d.Status == DimError || d.Status == DimInconclusive {
				base.OutcomeKind = contract.OutcomeInconclusive
				base.Scored = false
				base.Notes = "pass revoked: non-pass dimension present"
				break
			}
		}
	}
	return base, nil
}

func (c *Checker) scoreDimensions(env EvaluatorEnv, hidden HiddenSpec) ([]DimensionResult, error) {
	art := env.Artifact
	role := art.CaseRole

	out := make([]DimensionResult, 0, 4)

	// DimFailedTask — always applicable for failure role; for control roles
	// we still check task markers when present, else N/A is wrong — controls
	// have their own primary dimensions. For failure: grade task outcome.
	switch role {
	case contract.CaseRoleFailure:
		out = append(out, gradeFailedTask(art, hidden))
		out = append(out, gradeConstraint(env, art, hidden))
		out = append(out, gradeRegression(env, art, hidden))
		// no-relevant-memory dimension is N/A on the failure case itself
		out = append(out, DimensionResult{
			Dimension: DimNoRelevantMemory,
			Status:    DimNotApplicable,
			Evidence:  "role=failure",
			Detail:    "no-relevant-memory control is graded on its own case role",
		})
	case contract.CaseRoleValidMemoryControl:
		// Control focuses on constraint survival; task outcome still checked
		// when pass markers are defined, else N/A.
		ft := gradeFailedTask(art, hidden)
		if len(hidden.TaskPassMarkers) == 0 && len(hidden.TaskFailMarkers) == 0 {
			ft = DimensionResult{
				Dimension: DimFailedTask,
				Status:    DimNotApplicable,
				Evidence:  "role=valid_memory_control",
				Detail:    "failure-task markers not applicable to valid-memory control fixture",
			}
		}
		out = append(out, ft)
		out = append(out, gradeConstraint(env, art, hidden))
		out = append(out, gradeRegression(env, art, hidden))
		out = append(out, DimensionResult{
			Dimension: DimNoRelevantMemory,
			Status:    DimNotApplicable,
			Evidence:  "role=valid_memory_control",
			Detail:    "no-relevant-memory graded on its own case",
		})
	case contract.CaseRoleNoRelevantMemoryCtrl:
		out = append(out, DimensionResult{
			Dimension: DimFailedTask,
			Status:    DimNotApplicable,
			Evidence:  "role=no_relevant_memory_control",
			Detail:    "failed-task dimension graded on failure case",
		})
		out = append(out, DimensionResult{
			Dimension: DimValidMemoryConstraint,
			Status:    DimNotApplicable,
			Evidence:  "role=no_relevant_memory_control",
			Detail:    "valid-memory constraint graded on failure/valid-memory cases",
		})
		out = append(out, gradeRegression(env, art, hidden))
		out = append(out, gradeNoRelevantMemory(art, hidden))
	default:
		return nil, errf(CodeUnsupportedArtifact, "case_role", "unsupported role %q", role)
	}
	return out, nil
}

func gradeFailedTask(art OutputArtifact, hidden HiddenSpec) DimensionResult {
	// Known-fault markers: if any required fail marker is present as stated → fail.
	for _, m := range hidden.TaskFailMarkers {
		got, ok := art.TaskMarkers[m.Key]
		if m.MustBeAbsent {
			if ok && got == m.RequiredValue {
				return DimensionResult{
					Dimension: DimFailedTask,
					Status:    DimFail,
					Evidence:  "marker:" + m.Key,
					Detail:    fmt.Sprintf("known-fault marker %q still set to %q", m.Key, got),
				}
			}
			continue
		}
		if ok && got == m.RequiredValue {
			return DimensionResult{
				Dimension: DimFailedTask,
				Status:    DimFail,
				Evidence:  "marker:" + m.Key,
				Detail:    fmt.Sprintf("known-fault marker matched %q=%q", m.Key, got),
			}
		}
	}
	// Pass markers: all must match.
	for _, m := range hidden.TaskPassMarkers {
		got, ok := art.TaskMarkers[m.Key]
		if m.MustBeAbsent {
			if ok && got == m.RequiredValue {
				return DimensionResult{
					Dimension: DimFailedTask,
					Status:    DimFail,
					Evidence:  "marker:" + m.Key,
					Detail:    fmt.Sprintf("pass requires absence of %q=%q", m.Key, m.RequiredValue),
				}
			}
			continue
		}
		if !ok || got != m.RequiredValue {
			return DimensionResult{
				Dimension: DimFailedTask,
				Status:    DimFail,
				Evidence:  "marker:" + m.Key,
				Detail:    fmt.Sprintf("pass marker missing or mismatch: want %q=%q, got ok=%v val=%q", m.Key, m.RequiredValue, ok, got),
			}
		}
	}
	if len(hidden.TaskPassMarkers) == 0 && len(hidden.TaskFailMarkers) == 0 {
		return DimensionResult{
			Dimension: DimFailedTask,
			Status:    DimInconclusive,
			Evidence:  "no_markers",
			Detail:    "hidden spec defines no task markers",
		}
	}
	return DimensionResult{
		Dimension: DimFailedTask,
		Status:    DimPass,
		Evidence:  "task_markers",
		Detail:    "task pass markers satisfied; known-fault markers absent",
	}
}

func gradeConstraint(env EvaluatorEnv, art OutputArtifact, hidden HiddenSpec) DimensionResult {
	if len(hidden.ConstraintChecks) == 0 {
		return DimensionResult{
			Dimension: DimValidMemoryConstraint,
			Status:    DimInconclusive,
			Evidence:  "no_constraint_checks",
			Detail:    "hidden spec defines no constraint checks",
		}
	}
	for _, c := range hidden.ConstraintChecks {
		body, ok := art.FileContents[c.Path]
		if !ok {
			// Try reading from evaluator env files/ copy.
			b, err := os.ReadFile(filepath.Join(env.Root, "files", c.Path))
			if err != nil {
				return DimensionResult{
					Dimension: DimValidMemoryConstraint,
					Status:    DimFail,
					Evidence:  "file:" + c.Path,
					Detail:    fmt.Sprintf("constraint file missing: %s", c.Path),
				}
			}
			body = string(b)
		}
		contains := strings.Contains(body, c.Token)
		if c.MustContain && !contains {
			return DimensionResult{
				Dimension: DimValidMemoryConstraint,
				Status:    DimFail,
				Evidence:  "token:" + c.Token,
				Detail:    fmt.Sprintf("valid-memory constraint token %q missing from %s (%s)", c.Token, c.Path, c.Description),
			}
		}
		if !c.MustContain && contains {
			return DimensionResult{
				Dimension: DimValidMemoryConstraint,
				Status:    DimFail,
				Evidence:  "token:" + c.Token,
				Detail:    fmt.Sprintf("forbidden token %q present in %s", c.Token, c.Path),
			}
		}
	}
	return DimensionResult{
		Dimension: DimValidMemoryConstraint,
		Status:    DimPass,
		Evidence:  "constraint_checks",
		Detail:    "all valid-memory constraint tokens preserved",
	}
}

func gradeRegression(env EvaluatorEnv, art OutputArtifact, hidden HiddenSpec) DimensionResult {
	if len(hidden.RegressionGuards) == 0 {
		return DimensionResult{
			Dimension: DimUnrelatedRegression,
			Status:    DimNotApplicable,
			Evidence:  "no_regression_guards",
			Detail:    "no unrelated-regression guards defined for this synthetic suite",
		}
	}
	for _, g := range hidden.RegressionGuards {
		body, ok := art.FileContents[g.Path]
		if !ok {
			b, err := os.ReadFile(filepath.Join(env.Root, "files", g.Path))
			if err != nil {
				return DimensionResult{
					Dimension: DimUnrelatedRegression,
					Status:    DimFail,
					Evidence:  "file:" + g.Path,
					Detail:    fmt.Sprintf("regression guard file missing: %s", g.Path),
				}
			}
			body = string(b)
		}
		contains := strings.Contains(body, g.Token)
		if g.MustContain && !contains {
			return DimensionResult{
				Dimension: DimUnrelatedRegression,
				Status:    DimFail,
				Evidence:  "token:" + g.Token,
				Detail:    fmt.Sprintf("unrelated regression: missing %q in %s", g.Token, g.Path),
			}
		}
		if !g.MustContain && contains {
			return DimensionResult{
				Dimension: DimUnrelatedRegression,
				Status:    DimFail,
				Evidence:  "token:" + g.Token,
				Detail:    fmt.Sprintf("unrelated regression: forbidden %q in %s", g.Token, g.Path),
			}
		}
	}
	return DimensionResult{
		Dimension: DimUnrelatedRegression,
		Status:    DimPass,
		Evidence:  "regression_guards",
		Detail:    "unrelated regression guards held",
	}
}

func gradeNoRelevantMemory(art OutputArtifact, hidden HiddenSpec) DimensionResult {
	spec := hidden.NoRelevantMemory
	if len(spec.SuccessMarkers) == 0 && len(spec.ForbiddenMarkers) == 0 {
		return DimensionResult{
			Dimension: DimNoRelevantMemory,
			Status:    DimInconclusive,
			Evidence:  "no_control_markers",
			Detail:    "no-relevant-memory spec empty",
		}
	}
	for _, m := range spec.ForbiddenMarkers {
		got, ok := art.TaskMarkers[m.Key]
		if !m.MustBeAbsent {
			// Forbidden marker present as stated → fail (task required memory edit).
			if ok && got == m.RequiredValue {
				return DimensionResult{
					Dimension: DimNoRelevantMemory,
					Status:    DimFail,
					Evidence:  "marker:" + m.Key,
					Detail:    fmt.Sprintf("no-relevant-memory control failed: forbidden marker %q=%q (success must not require memory edit)", m.Key, got),
				}
			}
			continue
		}
		// MustBeAbsent form of forbidden marker.
		if ok && got == m.RequiredValue {
			return DimensionResult{
				Dimension: DimNoRelevantMemory,
				Status:    DimFail,
				Evidence:  "marker:" + m.Key,
				Detail:    fmt.Sprintf("forbidden marker present: %q=%q", m.Key, got),
			}
		}
	}
	for _, m := range spec.SuccessMarkers {
		got, ok := art.TaskMarkers[m.Key]
		if m.MustBeAbsent {
			if ok && got == m.RequiredValue {
				return DimensionResult{
					Dimension: DimNoRelevantMemory,
					Status:    DimFail,
					Evidence:  "marker:" + m.Key,
					Detail:    fmt.Sprintf("success requires absence of %q=%q", m.Key, m.RequiredValue),
				}
			}
			continue
		}
		if !ok || got != m.RequiredValue {
			return DimensionResult{
				Dimension: DimNoRelevantMemory,
				Status:    DimFail,
				Evidence:  "marker:" + m.Key,
				Detail:    fmt.Sprintf("no-relevant-memory success marker missing: want %q=%q", m.Key, m.RequiredValue),
			}
		}
	}
	return DimensionResult{
		Dimension: DimNoRelevantMemory,
		Status:    DimPass,
		Evidence:  "no_relevant_memory",
		Detail:    "control succeeded without requiring the proposed memory edit",
	}
}

func inconclusiveAll(evidence, detail string) []DimensionResult {
	dims := []string{
		DimFailedTask,
		DimValidMemoryConstraint,
		DimUnrelatedRegression,
		DimNoRelevantMemory,
	}
	out := make([]DimensionResult, 0, len(dims))
	for _, d := range dims {
		out = append(out, DimensionResult{
			Dimension: d,
			Status:    DimInconclusive,
			Evidence:  evidence,
			Detail:    detail,
		})
	}
	return out
}

func errorAll(evidence, detail string) []DimensionResult {
	dims := []string{
		DimFailedTask,
		DimValidMemoryConstraint,
		DimUnrelatedRegression,
		DimNoRelevantMemory,
	}
	out := make([]DimensionResult, 0, len(dims))
	for _, d := range dims {
		out = append(out, DimensionResult{
			Dimension: d,
			Status:    DimError,
			Evidence:  evidence,
			Detail:    detail,
		})
	}
	return out
}
