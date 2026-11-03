package contract

// CaseDocument is the frozen identity, eligibility and input package for one
// pilot case (failure or control). Hidden oracle material must not appear
// inside ContenderVisible paths.
type CaseDocument struct {
	Header
	CaseID               string           `json:"case_id"`
	Role                 string           `json:"role"`
	Eligibility          Eligibility      `json:"eligibility"`
	Cutoff               TimeCutoff       `json:"cutoff"`
	Repository           RepositoryState  `json:"repository"`
	Harness              HarnessConfig    `json:"harness"`
	MemorySnapshot       MemorySnapshot   `json:"memory_snapshot"`
	DeliveredContext     DeliveredContext `json:"delivered_context"`
	RunnerPackage        RunnerPackage    `json:"runner_package"`
	OraclePackage        OraclePackage    `json:"oracle_package"`
	Controls             ControlSpec      `json:"controls"`
	IsolationAssumptions []string         `json:"isolation_assumptions"`
	Notes                string           `json:"notes,omitempty"`
}

// NewCaseDocument returns a CaseDocument with schema header filled.
func NewCaseDocument() CaseDocument {
	return CaseDocument{Header: newHeader(SchemaCase)}
}

// Eligibility records go/no-go gates for historical attribution.
type Eligibility struct {
	EligibleForHistoricalAttribution bool     `json:"eligible_for_historical_attribution"`
	PermissionRecorded               bool     `json:"permission_recorded"`
	PrivatePackageOutsidePublicRepo  bool     `json:"private_package_outside_public_repo"`
	MissingInputs                    []string `json:"missing_inputs"`
	Decision                         string   `json:"decision"` // "eligible" | "no_go" | "prospective_capture"
	Rationale                        string   `json:"rationale,omitempty"`
}

// TimeCutoff is the temporal boundary: post-cutoff evidence is oracle-only.
type TimeCutoff struct {
	CutoffAt            string `json:"cutoff_at"` // RFC3339
	TimezoneNote        string `json:"timezone_note,omitempty"`
	PostCutoffForbidden bool   `json:"post_cutoff_forbidden_to_contender"`
}

// DirtyFile is one relevant working-tree change at the frozen commit.
type DirtyFile struct {
	Path       string `json:"path"`
	ContentSHA string `json:"content_sha256"`
	Status     string `json:"status"` // "modified" | "added" | "deleted" | "renamed"
}

// RepositoryState freezes the coding workspace the contender may see.
type RepositoryState struct {
	CommitSHA          string      `json:"commit_sha"`
	DirtyFiles         []DirtyFile `json:"dirty_files"`
	RelevantDirtyOnly  bool        `json:"relevant_dirty_only"`
	ReconstructionNote string      `json:"reconstruction_note,omitempty"`
}

// HarnessConfig freezes tool/model/harness settings. A change here is a
// separate condition or a declared confound — not silent under an edit.
type HarnessConfig struct {
	ModelID               string   `json:"model_id"`
	Provider              string   `json:"provider"`
	ProviderVersionStatus string   `json:"provider_version_status"`
	ProviderVersion       string   `json:"provider_version,omitempty"`
	ToolsetID             string   `json:"toolset_id"`
	HarnessID             string   `json:"harness_id"`
	HarnessVersion        string   `json:"harness_version"`
	Temperature           *float64 `json:"temperature,omitempty"`
	MaxOutputTokens       *int     `json:"max_output_tokens,omitempty"`
	Notes                 string   `json:"notes,omitempty"`
}

// MemorySnapshot is the stored memory bytes/version available at cutoff.
// It is NOT the delivered prompt — see DeliveredContext.
type MemorySnapshot struct {
	SnapshotID    string   `json:"snapshot_id"`
	Format        string   `json:"format"` // e.g. "mora_vault_export_v1" | "synthetic_v1"
	BytesSHA256   string   `json:"bytes_sha256"`
	ByteLength    int      `json:"byte_length"`
	MemoryVersion string   `json:"memory_version"`
	RecordIDs     []string `json:"record_ids"`
	Fidelity      string   `json:"fidelity"`
	Synthetic     bool     `json:"synthetic"`
	Notes         string   `json:"notes,omitempty"`
}

// DeliveredPart is one exact byte region the agent received (prompt, tool
// result, system message, etc.).
type DeliveredPart struct {
	PartID      string   `json:"part_id"`
	Kind        string   `json:"kind"` // "system" | "user" | "tool_result" | "memory_block" | "other"
	BytesSHA256 string   `json:"bytes_sha256"`
	ByteLength  int      `json:"byte_length"`
	MemoryRefs  []string `json:"memory_refs,omitempty"` // snapshot record ids if any
}

// DeliveredContext is the exposure manifest: exact delivered prompt and
// tool-result bytes, kept separate from MemorySnapshot.
type DeliveredContext struct {
	ManifestID           string          `json:"manifest_id"`
	ExposureAvailability string          `json:"exposure_availability"`
	Fidelity             string          `json:"fidelity"`
	Parts                []DeliveredPart `json:"parts"`
	MissingReason        string          `json:"missing_reason,omitempty"`
	Synthetic            bool            `json:"synthetic"`
}

// RunnerPackage is contender-visible material only. It must not contain
// hidden tests, expected answers, reference patches or post-cutoff evidence.
type RunnerPackage struct {
	PackageID               string   `json:"package_id"`
	ContenderVisiblePaths   []string `json:"contender_visible_paths"`
	ContainsHiddenTests     bool     `json:"contains_hidden_tests"`
	ContainsExpectedAnswers bool     `json:"contains_expected_answers"`
	ContainsReferencePatch  bool     `json:"contains_reference_patch"`
	ContainsPostCutoff      bool     `json:"contains_post_cutoff_evidence"`
	ContentMinimized        bool     `json:"content_minimized"`
}

// OraclePackage is inaccessible to contender and repair. Reference-memory
// ceiling is future optional work, not an initial condition.
type OraclePackage struct {
	PackageID               string   `json:"package_id"`
	HiddenTestRefs          []string `json:"hidden_test_refs"`
	ExpectedAnswerRefs      []string `json:"expected_answer_refs"`
	ReferencePatchRefs      []string `json:"reference_patch_refs"`
	PostCutoffEvidenceRefs  []string `json:"post_cutoff_evidence_refs"`
	ReferenceMemoryCeiling  bool     `json:"reference_memory_ceiling"` // must be false for initial pilot
	InaccessibleToContender bool     `json:"inaccessible_to_contender"`
	InaccessibleToRepair    bool     `json:"inaccessible_to_repair"`
}

// ControlSpec records both required controls for a failure case package.
// For control cases themselves, the matching constraint is restated.
type ControlSpec struct {
	ValidMemoryConstraint string `json:"valid_memory_constraint"`
	// ValidMemoryMustSurvive: the edit condition must not discard this constraint.
	ValidMemoryMustSurvive bool `json:"valid_memory_must_survive"`
	// NoRelevantMemoryTask: success must not require the proposed memory edit.
	NoRelevantMemoryTask        bool   `json:"no_relevant_memory_task"`
	NoRelevantMemorySuccessRule string `json:"no_relevant_memory_success_rule"`
}

// ConditionDocument freezes one experimental arm.
type ConditionDocument struct {
	Header
	ConditionID        string   `json:"condition_id"`
	CaseID             string   `json:"case_id"`
	Kind               string   `json:"kind"`
	MemoryEditRef      string   `json:"memory_edit_ref,omitempty"` // required for reviewed_memory_edit
	EditReviewedBy     string   `json:"edit_reviewed_by,omitempty"`
	HarnessUnchanged   bool     `json:"harness_unchanged"`
	RetrievalUnchanged bool     `json:"retrieval_unchanged"`
	DeclaredConfounds  []string `json:"declared_confounds"`
	Notes              string   `json:"notes,omitempty"`
}

// NewConditionDocument returns a ConditionDocument with schema header filled.
func NewConditionDocument() ConditionDocument {
	return ConditionDocument{Header: newHeader(SchemaCondition), DeclaredConfounds: []string{}}
}

// AttemptDocument records one run attempt under a condition.
type AttemptDocument struct {
	Header
	AttemptID       string `json:"attempt_id"`
	ConditionID     string `json:"condition_id"`
	CaseID          string `json:"case_id"`
	RepIndex        int    `json:"rep_index"`
	Status          string `json:"status"`
	StartedAt       string `json:"started_at,omitempty"`
	FinishedAt      string `json:"finished_at,omitempty"`
	TimeoutSeconds  int    `json:"timeout_seconds"`
	CostUSDMicros   int64  `json:"cost_usd_micros"`
	ProviderInvoked bool   `json:"provider_invoked"`
	SkipReason      string `json:"skip_reason,omitempty"`
	ErrorCode       string `json:"error_code,omitempty"`
	ResetObserved   bool   `json:"reset_observed"`
	IsolationHeld   bool   `json:"isolation_held"`
	Notes           string `json:"notes,omitempty"`
}

// NewAttemptDocument returns an AttemptDocument with schema header filled.
func NewAttemptDocument() AttemptDocument {
	return AttemptDocument{Header: newHeader(SchemaAttempt)}
}

// OutcomeDocument is the oracle judgment for one attempt.
type OutcomeDocument struct {
	Header
	OutcomeID string   `json:"outcome_id"`
	AttemptID string   `json:"attempt_id"`
	Kind      string   `json:"kind"`
	CheckerID string   `json:"checker_id"`
	Score     *float64 `json:"score,omitempty"`
	DetailRef string   `json:"detail_ref,omitempty"` // oracle-side only
	Notes     string   `json:"notes,omitempty"`
}

// NewOutcomeDocument returns an OutcomeDocument with schema header filled.
func NewOutcomeDocument() OutcomeDocument {
	return OutcomeDocument{Header: newHeader(SchemaOutcome)}
}

// CostAccounting aggregates spend for a report or run gate.
type CostAccounting struct {
	Currency           string `json:"currency"` // "USD"
	SpentUSDMicros     int64  `json:"spent_usd_micros"`
	CeilingUSDMicros   int64  `json:"ceiling_usd_micros"`
	FailClosed         bool   `json:"fail_closed"`
	ProviderCalls      int    `json:"provider_calls"`
	AccountingComplete bool   `json:"accounting_complete"`
}

// RetryPolicy describes retry and missing-run handling.
type RetryPolicy struct {
	MaxRetriesPerAttempt int    `json:"max_retries_per_attempt"`
	RetryOnTimeout       bool   `json:"retry_on_timeout"`
	RetryOnUnavailable   bool   `json:"retry_on_unavailable"`
	MissingRunPolicy     string `json:"missing_run_policy"` // "record_unavailable" | "exclude_with_note" | "fail_closed"
	Notes                string `json:"notes,omitempty"`
}

// ExecutionPolicy bundles reset, isolation, minimization, timeout and spend.
type ExecutionPolicy struct {
	ResetRequired         bool           `json:"reset_required"`
	IsolationRequired     bool           `json:"isolation_required"`
	ContentMinimization   bool           `json:"content_minimization"`
	DefaultTimeoutSeconds int            `json:"default_timeout_seconds"`
	Retry                 RetryPolicy    `json:"retry"`
	Cost                  CostAccounting `json:"cost"`
	DefaultDeny           []string       `json:"default_deny"`
}

// RunGate is checked before any provider invocation. Absent permission or
// spend ceiling fails closed.
type RunGate struct {
	Header
	GateID                string          `json:"gate_id"`
	RunPermissionGranted  bool            `json:"run_permission_granted"`
	PermissionRecordRef   string          `json:"permission_record_ref,omitempty"`
	MatrixFrozen          bool            `json:"matrix_frozen"`
	MonetaryCeilingFrozen bool            `json:"monetary_ceiling_frozen"`
	Policy                ExecutionPolicy `json:"policy"`
	ApprovedBy            string          `json:"approved_by,omitempty"`
	Notes                 string          `json:"notes,omitempty"`
}

// NewRunGate returns a RunGate with schema header filled.
func NewRunGate() RunGate {
	return RunGate{
		Header: newHeader(SchemaRunGate),
		Policy: ExecutionPolicy{
			DefaultDeny: []string{DenyProductionWrites, DenyExternalActions, DenyLiveVaultAccess},
		},
	}
}

// PlanningMatrix is the suggested default only. Actual matrix + monetary
// ceiling must freeze before model execution.
type PlanningMatrix struct {
	RepsPerCell          int    `json:"reps_per_cell"`
	FailureCases         int    `json:"failure_cases"`
	ControlCases         int    `json:"control_cases"`
	Conditions           int    `json:"conditions"`
	MaxPlannedRuns       int    `json:"max_planned_runs"`
	IsApprovedSpend      bool   `json:"is_approved_spend"`
	IsStatisticalClaim   bool   `json:"is_statistical_claim"`
	MustFreezeBeforeExec bool   `json:"must_freeze_before_exec"`
	Notes                string `json:"notes,omitempty"`
}

// DefaultPlanningMatrix returns the issue #541 suggested matrix (≤45 runs).
func DefaultPlanningMatrix() PlanningMatrix {
	return PlanningMatrix{
		RepsPerCell:          PlanningRepsPerCell,
		FailureCases:         PlanningFailureCases,
		ControlCases:         PlanningControlCases,
		Conditions:           PlanningConditions,
		MaxPlannedRuns:       PlanningMaxPlannedRuns,
		IsApprovedSpend:      false,
		IsStatisticalClaim:   false,
		MustFreezeBeforeExec: true,
		Notes:                "Planning default only. Freeze actual matrix and monetary ceiling before model execution. Not approved spend.",
	}
}

// AccessRow is one row of the isolation access table.
type AccessRow struct {
	Principal string `json:"principal"`
	Resource  string `json:"resource"`
	Access    string `json:"access"`
	Rationale string `json:"rationale"`
}

// ReportDocument aggregates attempts and outcomes for one case×condition set.
type ReportDocument struct {
	Header
	ReportID       string         `json:"report_id"`
	CaseIDs        []string       `json:"case_ids"`
	ConditionIDs   []string       `json:"condition_ids"`
	AttemptIDs     []string       `json:"attempt_ids"`
	OutcomeIDs     []string       `json:"outcome_ids"`
	Matrix         PlanningMatrix `json:"matrix"`
	Cost           CostAccounting `json:"cost"`
	AccessTable    []AccessRow    `json:"access_table"`
	Assumptions    []string       `json:"isolation_assumptions"`
	Limitations    []string       `json:"limitations"`
	ClaimsEfficacy bool           `json:"claims_efficacy"` // must be false for contract-only receipts
	Notes          string         `json:"notes,omitempty"`
}

// NewReportDocument returns a ReportDocument with schema header filled.
func NewReportDocument() ReportDocument {
	return ReportDocument{
		Header:         newHeader(SchemaReport),
		CaseIDs:        []string{},
		ConditionIDs:   []string{},
		AttemptIDs:     []string{},
		OutcomeIDs:     []string{},
		AccessTable:    []AccessRow{},
		Assumptions:    []string{},
		Limitations:    []string{},
		ClaimsEfficacy: false,
	}
}

// ContractReceipt is the short offline validation receipt. It must not claim
// downstream efficacy.
type ContractReceipt struct {
	Header
	ReceiptID        string            `json:"receipt_id"`
	GeneratedAt      string            `json:"generated_at"`
	ExampleHashes    map[string]string `json:"example_hashes"`
	TestsPassed      bool              `json:"tests_passed"`
	UnresolvedLimits []string          `json:"unresolved_limits"`
	ClaimsEfficacy   bool              `json:"claims_efficacy"`
	Notes            string            `json:"notes,omitempty"`
}

// NewContractReceipt returns a ContractReceipt with schema header filled.
func NewContractReceipt() ContractReceipt {
	return ContractReceipt{
		Header:           newHeader(SchemaReceipt),
		ExampleHashes:    map[string]string{},
		UnresolvedLimits: []string{},
		ClaimsEfficacy:   false,
	}
}
