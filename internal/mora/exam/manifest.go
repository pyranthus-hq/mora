package exam

const (
	RuleIdentity          = "identity"
	RuleTimestamp         = "timestamp"
	RuleTransition        = "transition"
	RuleDirection         = "direction"
	RuleClosure           = "closure"
	RuleEvidenceSpan      = "evidence_span"
	RuleReplyChainQuotes  = "reply_chain_quotes"
	RuleSelfAttendee      = "self_is_attendee"
	RuleOneDefectArtifact = "one_defect_per_artifact"
	RuleClassBalance      = "class_balance"
	RulePersonaHygiene    = "persona_hygiene"
	RuleChannelGrain      = "channel_grain_connector_survivability"

	LintRealIdentity = "real_identity_ledger"
	LintCorpusBytes  = "real_identity_corpus"
)

var RequiredValidatorRules = []string{
	RuleIdentity,
	RuleTimestamp,
	RuleTransition,
	RuleDirection,
	RuleClosure,
	RuleEvidenceSpan,
	RuleReplyChainQuotes,
	RuleSelfAttendee,
	RuleOneDefectArtifact,
	RuleClassBalance,
	RulePersonaHygiene,
	RuleChannelGrain,
}

var RequiredLints = []string{LintRealIdentity, LintCorpusBytes}
