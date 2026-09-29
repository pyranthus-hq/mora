package oracle

import (
	"encoding/json"
	"strings"

	"github.com/pyranthus-hq/mora/internal/mora/pilotreplay/contract"
)

// ContenderView is the contender-visible payload derived from a case. It must
// omit gold, reference patches, hidden checks and future/post-cutoff evidence.
type ContenderView struct {
	CaseID                string   `json:"case_id"`
	Role                  string   `json:"role"`
	ContenderVisiblePaths []string `json:"contender_visible_paths"`
	SnapshotID            string   `json:"snapshot_id"`
	DeliveredManifestID   string   `json:"delivered_manifest_id"`
	// OracleHashRefs are hash-ref only (no contents).
	OraclePackageID string   `json:"oracle_package_id,omitempty"`
	OracleHashRefs  []string `json:"oracle_hash_refs,omitempty"`
}

// RepairView is what a repair author may see. Same gold omissions as contender
// plus no hidden checker bodies.
type RepairView struct {
	CaseID                string   `json:"case_id"`
	Role                  string   `json:"role"`
	MemorySnapshotID      string   `json:"memory_snapshot_id"`
	ValidMemoryConstraint string   `json:"valid_memory_constraint,omitempty"`
	AllowedFeedbackHint   []string `json:"allowed_feedback_fields"`
}

// RunnerOracleStaging is what the runner may stage: hash-ref only.
type RunnerOracleStaging struct {
	PackageID       string   `json:"package_id"`
	HashRefs        []string `json:"hash_refs"`
	HashRefOnly     bool     `json:"hash_ref_only"`
	ContentsMounted bool     `json:"contents_mounted"` // must be false
}

// BuildContenderView projects a case into contender-visible fields only.
func BuildContenderView(c contract.CaseDocument) (ContenderView, error) {
	if err := assertCaseOracleIsolation(c); err != nil {
		return ContenderView{}, err
	}
	// Contender may learn that hash-refs exist via runner staging metadata,
	// but this view only carries package id + empty contents — never bodies.
	return ContenderView{
		CaseID:                c.CaseID,
		Role:                  c.Role,
		ContenderVisiblePaths: append([]string(nil), c.RunnerPackage.ContenderVisiblePaths...),
		SnapshotID:            c.MemorySnapshot.SnapshotID,
		DeliveredManifestID:   c.DeliveredContext.ManifestID,
		OraclePackageID:       c.OraclePackage.PackageID,
		OracleHashRefs:        nil, // bodies denied; even refs omitted from contender view
	}, nil
}

// BuildRepairView projects repair-author-visible fields only.
func BuildRepairView(c contract.CaseDocument) (RepairView, error) {
	if err := assertCaseOracleIsolation(c); err != nil {
		return RepairView{}, err
	}
	p := FrozenScoringPolicy()
	return RepairView{
		CaseID:                c.CaseID,
		Role:                  c.Role,
		MemorySnapshotID:      c.MemorySnapshot.SnapshotID,
		ValidMemoryConstraint: c.Controls.ValidMemoryConstraint,
		AllowedFeedbackHint:   append([]string(nil), p.AllowedFeedbackFields...),
	}, nil
}

// BuildRunnerOracleStaging returns hash-ref-only staging metadata.
func BuildRunnerOracleStaging(c contract.CaseDocument) (RunnerOracleStaging, error) {
	if err := assertCaseOracleIsolation(c); err != nil {
		return RunnerOracleStaging{}, err
	}
	refs := append([]string{}, c.OraclePackage.HiddenTestRefs...)
	refs = append(refs, c.OraclePackage.ExpectedAnswerRefs...)
	refs = append(refs, c.OraclePackage.ReferencePatchRefs...)
	refs = append(refs, c.OraclePackage.PostCutoffEvidenceRefs...)
	return RunnerOracleStaging{
		PackageID:       c.OraclePackage.PackageID,
		HashRefs:        refs,
		HashRefOnly:     true,
		ContentsMounted: false,
	}, nil
}

func assertCaseOracleIsolation(c contract.CaseDocument) error {
	rp := c.RunnerPackage
	if rp.ContainsHiddenTests || rp.ContainsExpectedAnswers || rp.ContainsReferencePatch || rp.ContainsPostCutoff {
		return errf(CodeIsolationBreach, "runner_package", "contender-visible package must not contain hidden/oracle/post-cutoff material")
	}
	op := c.OraclePackage
	if !op.InaccessibleToContender || !op.InaccessibleToRepair {
		return errf(CodeIsolationBreach, "oracle_package", "oracle must be inaccessible to contender and repair")
	}
	if op.ReferenceMemoryCeiling {
		return errf(CodeIsolationBreach, "oracle_package", "reference-memory ceiling forbidden as initial condition")
	}
	for _, p := range rp.ContenderVisiblePaths {
		lp := strings.ToLower(p)
		for _, bad := range []string{"oracle", "gold", "hidden_test", "expected_answer", "reference_patch", "post_cutoff"} {
			if strings.Contains(lp, bad) {
				return errf(CodeGoldLeak, "contender_visible_paths", "path %q looks like oracle/gold material", p)
			}
		}
	}
	return nil
}

// AssertNoGoldInJSON fails if serialized view bytes contain forbidden substrings.
func AssertNoGoldInJSON(label string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return errf(CodeInternal, label, "marshal: %v", err)
	}
	s := strings.ToLower(string(b))
	// Structural field names like "oracle_package_id" / "oracle_hash_refs" are
	// allowed metadata labels; forbid gold *bodies* and explicit leak markers.
	forbidden := []string{
		`"gold_labels"`,
		`"expected_answer_body"`,
		`"hidden_test_body"`,
		`"reference_patch_body"`,
		`"post_cutoff_body"`,
		`"hidden_spec"`,
		"idempotency-key-GOLD-SECRET", // synthetic canary used in tests
		"PRIVATE_GOLD_MARKER",
	}
	for _, f := range forbidden {
		if strings.Contains(s, strings.ToLower(f)) {
			return errf(CodeGoldLeak, label, "forbidden material %q present in %s view", f, label)
		}
	}
	return nil
}

// AssertAllowedFeedbackClean checks feedback omits detail/gold bodies.
func AssertAllowedFeedbackClean(fb AllowedFeedback) error {
	b, err := json.Marshal(fb)
	if err != nil {
		return errf(CodeInternal, "allowed_feedback", "marshal: %v", err)
	}
	s := string(b)
	for _, bad := range []string{"Detail", "hidden_spec", "PRIVATE_GOLD", "expected_answer_body", "reference_patch"} {
		if strings.Contains(s, bad) {
			return errf(CodeGoldLeak, "allowed_feedback", "forbidden substring %q in allowed feedback", bad)
		}
	}
	// Ensure only declared fields conceptually — DimensionStatuses values are
	// status enums only.
	for dim, st := range fb.DimensionStatuses {
		switch st {
		case DimPass, DimFail, DimInconclusive, DimError, DimNotApplicable:
			// ok
		default:
			return errf(CodeInvalidInput, "dimension_statuses."+dim, "unexpected status %q", st)
		}
	}
	return nil
}
