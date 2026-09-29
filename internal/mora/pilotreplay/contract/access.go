package contract

// DefaultAccessTable returns the isolation access table for the initial pilot.
// Contender and repair never see oracle material; runner may hash-ref only.
func DefaultAccessTable() []AccessRow {
	return []AccessRow{
		{Principal: PrincipalContender, Resource: "memory_snapshot", Access: AccessAllow, Rationale: "condition-selected snapshot bytes only"},
		{Principal: PrincipalContender, Resource: "delivered_context", Access: AccessAllow, Rationale: "exact exposure for the attempt condition"},
		{Principal: PrincipalContender, Resource: "runner_package", Access: AccessAllow, Rationale: "minimized contender-visible task inputs"},
		{Principal: PrincipalContender, Resource: "oracle_hidden_tests", Access: AccessDeny, Rationale: "runner/oracle separation"},
		{Principal: PrincipalContender, Resource: "oracle_expected_answers", Access: AccessDeny, Rationale: "runner/oracle separation"},
		{Principal: PrincipalContender, Resource: "oracle_reference_patches", Access: AccessDeny, Rationale: "runner/oracle separation"},
		{Principal: PrincipalContender, Resource: "post_cutoff_evidence", Access: AccessDeny, Rationale: "temporal cutoff"},
		{Principal: PrincipalContender, Resource: "live_vault", Access: AccessDeny, Rationale: "default deny live-vault access"},
		{Principal: PrincipalContender, Resource: "production_writes", Access: AccessDeny, Rationale: "default deny production writes"},
		{Principal: PrincipalContender, Resource: "external_actions", Access: AccessDeny, Rationale: "default deny external actions"},

		{Principal: PrincipalRepair, Resource: "memory_snapshot", Access: AccessAllow, Rationale: "may propose a reviewed memory edit under condition"},
		{Principal: PrincipalRepair, Resource: "oracle_hidden_tests", Access: AccessDeny, Rationale: "repair must not see the checker"},
		{Principal: PrincipalRepair, Resource: "oracle_expected_answers", Access: AccessDeny, Rationale: "repair must not see expected answers"},
		{Principal: PrincipalRepair, Resource: "oracle_reference_patches", Access: AccessDeny, Rationale: "repair must not see reference patches"},
		{Principal: PrincipalRepair, Resource: "post_cutoff_evidence", Access: AccessDeny, Rationale: "temporal cutoff"},
		{Principal: PrincipalRepair, Resource: "live_vault", Access: AccessDeny, Rationale: "default deny live-vault access"},

		{Principal: PrincipalOracle, Resource: "oracle_hidden_tests", Access: AccessAllow, Rationale: "oracle scores outcomes"},
		{Principal: PrincipalOracle, Resource: "oracle_expected_answers", Access: AccessAllow, Rationale: "oracle scores outcomes"},
		{Principal: PrincipalOracle, Resource: "oracle_reference_patches", Access: AccessAllow, Rationale: "oracle may compare patches"},
		{Principal: PrincipalOracle, Resource: "post_cutoff_evidence", Access: AccessAllow, Rationale: "oracle-only temporal evidence"},
		{Principal: PrincipalOracle, Resource: "contender_workspace", Access: AccessHashRef, Rationale: "oracle may hash-ref contender outputs without mutating them"},

		{Principal: PrincipalRunner, Resource: "runner_package", Access: AccessAllow, Rationale: "runner stages minimized inputs"},
		{Principal: PrincipalRunner, Resource: "oracle_package", Access: AccessHashRef, Rationale: "runner stages oracle refs without exposing contents to contender"},
		{Principal: PrincipalRunner, Resource: "live_vault", Access: AccessDeny, Rationale: "default deny live-vault access"},
		{Principal: PrincipalRunner, Resource: "production_writes", Access: AccessDeny, Rationale: "default deny production writes"},
		{Principal: PrincipalRunner, Resource: "external_actions", Access: AccessDeny, Rationale: "default deny external actions"},

		{Principal: PrincipalOperator, Resource: "run_gate", Access: AccessAllow, Rationale: "operator freezes matrix and spend ceiling"},
		{Principal: PrincipalOperator, Resource: "permission_record", Access: AccessAllow, Rationale: "operator records run permission"},
		{Principal: PrincipalOperator, Resource: "private_case_package", Access: AccessRedact, Rationale: "private package stays outside public artifacts"},
	}
}

// DefaultIsolationAssumptions lists explicit assumptions the pilot relies on.
// Violating any assumption voids historical attribution for that attempt.
func DefaultIsolationAssumptions() []string {
	return []string{
		"Contender and repair processes cannot read oracle_package contents.",
		"Post-cutoff commits, patches, tests and chatter are unavailable to contender and repair.",
		"MemorySnapshot bytes are not substituted for DeliveredContext parts; missing exposure is marked explicitly.",
		"Provider/model version gaps are marked provider_version_status=missing|approximate; reconstruction is never upgraded to faithful_historical.",
		"Workspace is reset between attempts; residual state is an isolation breach.",
		"No production writes, external side-effects or live-vault reads occur during an attempt.",
		"Content minimization: only task-relevant dirty files and memory are staged.",
		"Reference-memory ceiling is out of scope for the initial three conditions.",
		"Changing retrieval or harness mid-matrix is a new condition or a declared confound.",
		"Public fixtures contain synthetic material only; private case paths and credentials never appear in-repo.",
	}
}

// ContenderDeniedResources lists resources that must remain AccessDeny for the contender.
func ContenderDeniedResources() []string {
	return []string{
		"oracle_hidden_tests",
		"oracle_expected_answers",
		"oracle_reference_patches",
		"post_cutoff_evidence",
		"live_vault",
		"production_writes",
		"external_actions",
	}
}
